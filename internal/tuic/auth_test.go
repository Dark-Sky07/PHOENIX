package tuic

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestUserRegistryBasic(t *testing.T) {
	reg := NewUserRegistry()
	testUUID := uuid.New()

	reg.SetUsers([]TuicClientSettings{
		{
			UUID:     testUUID.String(),
			Password: "supersecretpassword",
			Email:    "test@example.com",
		},
	})

	var fakeUUID [16]byte
	copy(fakeUUID[:], testUUID[:])

	var unknownUUID [16]byte
	_, _ = rand.Read(unknownUUID[:])
	_, err := reg.Authenticate(&tls.ConnectionState{}, unknownUUID, [32]byte{})
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("expected ErrUserNotFound, got %v", err)
	}
}

func TestTrafficCounterSaturatesAndPreservesInvalidEvidence(t *testing.T) {
	var counter atomic.Int64
	counter.Store(math.MaxInt64 - 2)
	addTrafficCounter(&counter, 3)
	addTrafficCounter(&counter, 5)
	if got := counter.Load(); got != math.MaxInt64 {
		t.Fatalf("counter wrapped: %d", got)
	}
	counter.Store(-7)
	addTrafficCounter(&counter, 10)
	if got := counter.Load(); got != -7 {
		t.Fatalf("invalid counter evidence overwritten: %d", got)
	}
}

func TestTrafficCounterConcurrentDrainDoesNotLoseIncrements(t *testing.T) {
	var counter atomic.Int64
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 10000; j++ {
				addTrafficCounter(&counter, 1)
			}
		}()
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	var total int64
	for {
		total += counter.Swap(0)
		select {
		case <-done:
			total += counter.Swap(0)
			if total != 80000 {
				t.Fatalf("increment loss across poll: got %d", total)
			}
			return
		default:
		}
	}
}

func TestRetiredUserLastSessionEndSerializesWithCollection(t *testing.T) {
	reg := NewUserRegistry()
	id := uuid.New()
	reg.SetUsers([]TuicClientSettings{{UUID: id.String(), Email: "retired@example.test"}})
	user := reg.users[id]
	user.sessions.Add(1)
	reg.SetUsers(nil)

	// Model a collector between its counter swaps and its retired-user check.
	reg.mu.Lock()
	done := make(chan struct{})
	go func() { reg.sessionEnded(user); close(done) }()
	select {
	case <-done:
		reg.mu.Unlock()
		t.Fatal("session ended while collector held registry lock")
	case <-time.After(20 * time.Millisecond):
	}
	// A relay can complete a write during collection. The session must remain
	// visible until the collector finishes so the next poll can drain it.
	user.Traffic.BytesUp.Add(17)
	if user.sessions.Load() != 1 {
		reg.mu.Unlock()
		t.Fatal("last session disappeared during collection")
	}
	reg.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session end did not complete")
	}
	got := reg.CollectTrafficDeltas()
	if len(got) != 1 || got[0].Up != 17 {
		t.Fatalf("late relay traffic lost: %+v", got)
	}
}

func TestUserRegistryPreservesNegativeCounterEvidence(t *testing.T) {
	reg := NewUserRegistry()
	reg.SetUsers([]TuicClientSettings{{UUID: uuid.NewString(), Email: "negative@example.test", TrafficID: 5}})
	if !reg.AddTestTraffic("negative@example.test", -7, -3) {
		t.Fatal("missing user")
	}
	got := reg.CollectTrafficDeltas()
	if len(got) != 1 || got[0].Up != -7 || got[0].Down != -3 {
		t.Fatalf("negative counter evidence lost: %+v", got)
	}
}

func TestUserRegistryCredentialUpdatesKeepOldCounters(t *testing.T) {
	reg := NewUserRegistry()
	u1 := uuid.New().String()
	u2 := uuid.New().String()

	reg.SetUsers([]TuicClientSettings{
		{UUID: u1, Password: "pass1", Email: "u1@example.com"},
		{UUID: u2, Password: "pass2", Email: "u2@example.com"},
	})

	reg.AddTestTraffic("u1@example.com", 100, 200)

	parsedU1, _ := uuid.Parse(u1)
	user1Before := reg.users[parsedU1]
	if user1Before == nil {
		t.Fatalf("expected user1 in registry")
	}

	revoked := reg.SetUsers([]TuicClientSettings{
		{UUID: u1, Password: "newpassword", Email: "u1-new@example.com"},
		{UUID: u2, Password: "pass2", Email: "u2@example.com"},
	})

	if len(revoked) != 1 || revoked[0] != user1Before {
		t.Fatalf("expected changed user snapshot to be retired, got %+v", revoked)
	}

	user1After := reg.users[parsedU1]
	if user1Before == user1After {
		t.Fatal("expected immutable user snapshot to be replaced")
	}
	if user1After.Password != "newpassword" || user1After.Email != "u1-new@example.com" {
		t.Fatalf("expected updated password and email, got %s, %s", user1After.Password, user1After.Email)
	}

	deltas := reg.CollectTrafficDeltas()
	if len(deltas) != 1 || deltas[0].Email != "u1@example.com" || deltas[0].Up != 100 || deltas[0].Down != 200 {
		t.Fatalf("expected preserved traffic deltas, got %+v", deltas)
	}
}

func TestUserRegistryRevocation(t *testing.T) {
	reg := NewUserRegistry()
	u1 := uuid.New().String()
	u2 := uuid.New().String()

	reg.SetUsers([]TuicClientSettings{
		{UUID: u1, Password: "pass1", Email: "u1@example.com"},
		{UUID: u2, Password: "pass2", Email: "u2@example.com"},
	})

	// Remove u1, keep only u2
	revoked := reg.SetUsers([]TuicClientSettings{
		{UUID: u2, Password: "pass2", Email: "u2@example.com"},
	})

	if len(revoked) != 1 || revoked[0].Email != "u1@example.com" {
		t.Fatalf("expected u1 revoked, got %+v", revoked)
	}

	parsedU1, _ := uuid.Parse(u1)
	if _, exists := reg.users[parsedU1]; exists {
		t.Fatalf("expected u1 removed from registry")
	}
}

func TestUserRegistryRetainsRevokedTrafficUntilSessionsFinish(t *testing.T) {
	reg := NewUserRegistry()
	uuidStr := uuid.New().String()
	reg.SetUsers([]TuicClientSettings{{UUID: uuidStr, Password: "p", Email: "revoked@example.test"}})
	parsed, _ := uuid.Parse(uuidStr)
	user := reg.users[parsed]
	user.sessions.Store(1)
	user.Traffic.BytesUp.Store(11)
	user.Traffic.BytesDown.Store(22)
	reg.SetUsers(nil)

	if got := reg.CollectTrafficDeltas(); len(got) != 1 || got[0].Email != user.Email || got[0].Up != 11 || got[0].Down != 22 {
		t.Fatalf("revoked traffic delta = %+v", got)
	}
	user.Traffic.BytesUp.Add(3)
	if got := reg.CollectTrafficDeltas(); len(got) != 1 || got[0].Up != 3 {
		t.Fatalf("final active-session delta = %+v", got)
	}
	reg.sessionEnded(user)
	if got := reg.CollectTrafficDeltas(); len(got) != 0 {
		t.Fatalf("empty retired user produced another delta: %+v", got)
	}
	if len(reg.retired) != 0 {
		t.Fatalf("finished user remained retired: %+v", reg.retired)
	}
}

func TestUserRegistryConcurrentCredentialUpdatesAndAuthentication(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t)
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("tls.X509KeyPair: %v", err)
	}
	clientRaw, serverRaw := net.Pipe()
	clientConn := tls.Client(clientRaw, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13})
	serverConn := tls.Server(serverRaw, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13})
	serverHandshake := make(chan error, 1)
	go func() { serverHandshake <- serverConn.Handshake() }()
	if err := clientConn.Handshake(); err != nil {
		t.Fatalf("client TLS handshake: %v", err)
	}
	if err := <-serverHandshake; err != nil {
		t.Fatalf("server TLS handshake: %v", err)
	}
	t.Cleanup(func() {
		_ = clientConn.Close()
		_ = serverConn.Close()
	})
	state := clientConn.ConnectionState()
	if !state.HandshakeComplete {
		t.Fatal("TLS handshake did not complete")
	}

	reg := NewUserRegistry()
	uuidStr := uuid.New().String()
	parsed, _ := uuid.Parse(uuidStr)
	reg.SetUsers([]TuicClientSettings{{UUID: uuidStr, Password: "initial", Email: "user@example.test"}})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			password := "a"
			if i%2 == 0 {
				password = "b"
			}
			reg.SetUsers([]TuicClientSettings{{UUID: uuidStr, Password: password, Email: "user@example.test"}})
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			_, _ = reg.Authenticate(&state, parsed, [32]byte{})
		}
	}()
	wg.Wait()
}
