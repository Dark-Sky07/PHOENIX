package tuic

import (
	"context"
	"crypto/tls"
	"fmt"
	"math"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/apernet/quic-go"

	"github.com/Dark-Sky07/phoenix/internal/logger"
)

func TestEnsureCannotConfuseDelimiterContainingUserSets(t *testing.T) {
	cert, key := generateTestCert(t)
	const a = "a0000000-0000-0000-0000-000000000001"
	const b = "a0000000-0000-0000-0000-000000000002"
	inst := Instance{Id: 44, Tag: "collision-test", Listen: "127.0.0.1", Port: 0, Certificate: string(cert), PrivateKey: string(key), Clients: []TuicClientSettings{
		{UUID: a, Email: "a", Password: "p"},
		{UUID: b, Email: "b", Password: "q"},
	}}
	m := &Manager{servers: map[int]*managed{}, lastStartErr: map[int]string{}}
	t.Cleanup(m.StopAll)
	if err := m.Ensure(inst); err != nil {
		t.Fatal(err)
	}
	server := m.servers[inst.Id].server
	changed := inst
	changed.Clients = []TuicClientSettings{{UUID: a, Email: "a", Password: "p|0:b=" + b + ":q"}}
	if inst.UsersFingerprint() == changed.UsersFingerprint() {
		t.Fatal("different user lists have identical fingerprint")
	}
	if err := m.Ensure(changed); err != nil {
		t.Fatal(err)
	}
	if server != m.servers[inst.Id].server {
		t.Fatal("client update restarted listener")
	}
	server.users.mu.RLock()
	defer server.users.mu.RUnlock()
	if len(server.users.users) != 1 {
		t.Fatalf("removed user is still registered: %d users", len(server.users.users))
	}
	for _, u := range server.users.users {
		if u.UUIDStr != a || u.Password != changed.Clients[0].Password {
			t.Fatalf("unexpected retained user: %s", u.UUIDStr)
		}
	}
}

func TestEnsureStartsServerAndReconciles(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t)

	// Find free UDP port
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()

	inst := Instance{
		Id:          11,
		Tag:         "tuic-11",
		Listen:      "127.0.0.1",
		Port:        port,
		Certificate: string(certPEM),
		PrivateKey:  string(keyPEM),
		Clients:     []TuicClientSettings{{UUID: "a0000000-0000-0000-0000-000000000001", Password: "p", Email: "e1"}},
	}

	m := &Manager{servers: map[int]*managed{}, lastStartErr: map[int]string{}}
	t.Cleanup(m.StopAll)

	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	if !m.HasRunning() {
		t.Fatal("expected manager to have running server")
	}

	// Port should be taken by the server
	if c, err := net.ListenPacket("udp", inst.BindTo()); err == nil {
		_ = c.Close()
		t.Fatal("expected server to be listening on port")
	}

	// Reconcile with empty list should remove it
	m.Reconcile([]Instance{})
	if m.HasRunning() {
		t.Fatal("expected no running servers after reconcile empty")
	}

	// Port should now be released
	c, err := net.ListenPacket("udp", inst.BindTo())
	if err != nil {
		t.Fatalf("expected port to be free after reconcile: %v", err)
	}
	_ = c.Close()
}

func TestEnsureHotUpdatesUsersWithoutRestart(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()

	inst := Instance{
		Id:          12,
		Tag:         "tuic-12",
		Listen:      "127.0.0.1",
		Port:        port,
		Certificate: string(certPEM),
		PrivateKey:  string(keyPEM),
		Clients:     []TuicClientSettings{{UUID: "a0000000-0000-0000-0000-000000000001", Password: "p1", Email: "e1"}},
	}

	m := &Manager{servers: map[int]*managed{}, lastStartErr: map[int]string{}}
	t.Cleanup(m.StopAll)

	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	server1 := m.servers[12].server

	// Update user list without changing port or certs
	inst.Clients = append(inst.Clients, TuicClientSettings{
		UUID:     "a0000000-0000-0000-0000-000000000002",
		Password: "p2",
		Email:    "e2",
	})

	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure with updated clients failed: %v", err)
	}

	server2 := m.servers[12].server
	if server1 != server2 {
		t.Fatal("expected server instance to be reused across user updates (zero-downtime hot update)")
	}

	// Verify both users are now in user registry
	if len(server2.users.users) != 2 {
		t.Fatalf("expected 2 users in registry, got %d", len(server2.users.users))
	}
}

func TestEnsureUpdatesTagWithoutRestart(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()

	inst := Instance{
		Id:          13,
		Tag:         "old-tag",
		Listen:      "127.0.0.1",
		Port:        port,
		Certificate: string(certPEM),
		PrivateKey:  string(keyPEM),
		Clients:     []TuicClientSettings{{UUID: "a0000000-0000-0000-0000-000000000001", Password: "p", Email: "e"}},
	}

	m := &Manager{servers: map[int]*managed{}, lastStartErr: map[int]string{}}
	t.Cleanup(m.StopAll)

	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	inst.Tag = "new-tag"
	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure updated tag failed: %v", err)
	}

	m.mu.Lock()
	gotTag := m.servers[13].tag
	m.mu.Unlock()
	if gotTag != "new-tag" {
		t.Fatalf("manager tag = %q, want %q", gotTag, "new-tag")
	}
}

func TestEnsureUpdatesControllerWithoutRestartForNewConnections(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()

	inst := Instance{
		Id:                    14,
		Tag:                   "tuic-controller-reload",
		Listen:                "127.0.0.1",
		Port:                  port,
		Certificate:           string(certPEM),
		PrivateKey:            string(keyPEM),
		CongestionControl:     "bbr",
		LogLevel:              "debug",
		MaxIdleTime:           30,
		AuthenticationTimeout: 30,
		Clients:               []TuicClientSettings{{UUID: "a0000000-0000-0000-0000-000000000001", Password: "p", Email: "e"}},
	}
	m := &Manager{servers: map[int]*managed{}, lastStartErr: map[int]string{}}
	t.Cleanup(m.StopAll)
	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	server := m.servers[inst.Id].server

	dial := func() *quic.Conn {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		conn, err := quic.DialAddr(ctx, server.packetConn.LocalAddr().String(), &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h3"},
		}, &quic.Config{EnableDatagrams: true, MaxIdleTimeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("QUIC dial failed: %v", err)
		}
		return conn
	}

	connBBR := dial()
	defer connBBR.CloseWithError(0, "")
	waitForTuicLog(t, "applied bbr congestion controller")
	bbrSender := waitForClientCongestionSender(t, server, connBBR, "bbr")

	inst.CongestionControl = "cubic"
	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure after CUBIC update failed: %v", err)
	}
	if m.servers[inst.Id].server != server {
		t.Fatal("changing congestion control restarted the listener")
	}
	if err := connBBR.Context().Err(); err != nil {
		t.Fatalf("existing BBR connection closed after controller update: %v", err)
	}
	if sender := congestionSenderForClient(t, server, connBBR); sender != bbrSender {
		t.Fatal("existing connection's BBR sender changed after hot update")
	}
	connCubic := dial()
	defer connCubic.CloseWithError(0, "")
	waitForTuicLog(t, "cubic is not available; applied new_reno congestion controller")
	cubicSender := waitForClientCongestionSender(t, server, connCubic, "new_reno")

	inst.CongestionControl = "new_reno"
	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure after New Reno update failed: %v", err)
	}
	if err := connBBR.Context().Err(); err != nil {
		t.Fatalf("existing BBR connection closed after second update: %v", err)
	}
	if err := connCubic.Context().Err(); err != nil {
		t.Fatalf("existing CUBIC connection closed after second update: %v", err)
	}
	if sender := congestionSenderForClient(t, server, connBBR); sender != bbrSender {
		t.Fatal("existing connection's BBR sender changed after second hot update")
	}
	if sender := congestionSenderForClient(t, server, connCubic); sender != cubicSender {
		t.Fatal("existing connection's CUBIC sender changed after second hot update")
	}
	connReno := dial()
	defer connReno.CloseWithError(0, "")
	waitForTuicLog(t, "applied new_reno congestion controller")
	waitForClientCongestionSender(t, server, connReno, "new_reno")
}

func waitForClientCongestionSender(t *testing.T, server *Server, client *quic.Conn, want string) uintptr {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	var observed []string
	seen := make(map[string]struct{})
	for time.Now().Before(deadline) {
		server.connectionsMu.Lock()
		for conn := range server.connections {
			remote := conn.RemoteAddr().String()
			actual, sender := inspectCongestionSender(conn)
			description := fmt.Sprintf("remote=%s controller=%s sender=%x", remote, actual, sender)
			if _, exists := seen[description]; !exists {
				seen[description] = struct{}{}
				observed = append(observed, description)
			}
			if !matchesClientSocket(conn, client) {
				continue
			}
			if actual == want {
				server.connectionsMu.Unlock()
				return sender
			}
		}
		server.connectionsMu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("server connection did not install %s congestion sender for client %s (observed %v)", want, client.LocalAddr(), observed)
	return 0
}

func congestionSenderForClient(t *testing.T, server *Server, client *quic.Conn) uintptr {
	t.Helper()
	server.connectionsMu.Lock()
	defer server.connectionsMu.Unlock()
	for conn := range server.connections {
		if matchesClientSocket(conn, client) {
			_, sender := inspectCongestionSender(conn)
			return sender
		}
	}
	t.Fatal("server connection for client is not registered")
	return 0
}

func matchesClientSocket(serverConn, clientConn *quic.Conn) bool {
	serverAddr, serverOK := serverConn.RemoteAddr().(*net.UDPAddr)
	clientAddr, clientOK := clientConn.LocalAddr().(*net.UDPAddr)
	return serverOK && clientOK && serverAddr.Port == clientAddr.Port
}

// lockedCongestion reads the sender under the mutex SetCongestionControl writes
// it under, which the BBR install after the handshake races otherwise.
func lockedCongestion(conn *quic.Conn) (reflect.Value, func()) {
	handler := reflect.ValueOf(conn).Elem().FieldByName("sentPacketHandler").Elem().Elem()
	mu := (*sync.RWMutex)(unsafe.Pointer(handler.FieldByName("congestionMutex").UnsafeAddr()))
	mu.RLock()
	return handler.FieldByName("congestion").Elem(), mu.RUnlock
}

func inspectCongestionSender(conn *quic.Conn) (string, uintptr) {
	controller, unlock := lockedCongestion(conn)
	defer unlock()
	sender := controller
	if controller.Type().String() == "*ackhandler.ccAdapterEx" || controller.Type().String() == "*ackhandler.ccAdapter" {
		sender = controller.Elem().FieldByName("CC").Elem()
	}
	switch {
	case strings.Contains(sender.Type().String(), "bbrSender"):
		return "bbr", sender.Pointer()
	case strings.Contains(sender.Type().String(), "cubicSender"):
		if sender.Elem().FieldByName("reno").Bool() {
			return "new_reno", sender.Pointer()
		}
		return "cubic", sender.Pointer()
	}
	return sender.Type().String(), sender.Pointer()
}

func waitForTuicLog(t *testing.T, message string) {
	t.Helper()
	marker := "inbound 14 (tuic-controller-reload): " + message
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(strings.Join(logger.GetLogs(10000, "DEBUG"), "\n"), marker) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for TUIC log %q", marker)
}

func TestCollectAllTraffic(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()

	inst := Instance{
		Id:          20,
		Tag:         "tuic-20",
		Listen:      "127.0.0.1",
		Port:        port,
		Certificate: string(certPEM),
		PrivateKey:  string(keyPEM),
		Clients:     []TuicClientSettings{{UUID: "a0000000-0000-0000-0000-000000000001", Password: "p", Email: "e1"}},
	}

	m := &Manager{servers: map[int]*managed{}, lastStartErr: map[int]string{}}
	t.Cleanup(m.StopAll)

	if err := m.Ensure(inst); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	if !m.AddTestTraffic(20, "e1", 500, 1000) {
		t.Fatal("AddTestTraffic failed")
	}

	inbounds, clients := m.CollectAllTraffic()
	if len(inbounds) != 1 || inbounds[0].Up != 500 || inbounds[0].Down != 1000 {
		t.Fatalf("unexpected inbounds: %+v", inbounds)
	}
	if len(clients) != 1 || clients[0].Up != 500 || clients[0].Down != 1000 || clients[0].Email != "e1" {
		t.Fatalf("unexpected clients: %+v", clients)
	}

	// Subsequent call returns empty deltas
	inbounds2, clients2 := m.CollectAllTraffic()
	if len(inbounds2) != 0 || len(clients2) != 0 {
		t.Fatalf("expected empty deltas after drain, got %+v, %+v", inbounds2, clients2)
	}
}

func TestManagerRetainsDistinctPendingSourceIdentities(t *testing.T) {
	m := &Manager{servers: make(map[int]*managed)}
	original := ClientTrafficDelta{TrafficID: 3, InboundID: 1, UUID: "old", Email: "old@example.test", Up: 10}
	replacement := ClientTrafficDelta{TrafficID: 3, InboundID: 1, UUID: "new", Email: "new@example.test", Down: 20}
	m.RequeueClientTraffic([]ClientTrafficDelta{original, replacement})
	_, got := m.CollectAllTraffic()
	if len(got) != 2 {
		t.Fatalf("distinct source evidence merged: %+v", got)
	}
	seen := make(map[ClientTrafficDelta]bool)
	for _, delta := range got {
		seen[delta] = true
	}
	if !seen[original] || !seen[replacement] {
		t.Fatalf("source identities changed: %+v", got)
	}
}

func TestManagerRequeuedTrafficCannotOverflow(t *testing.T) {
	m := &Manager{servers: make(map[int]*managed)}
	m.RequeueClientTraffic([]ClientTrafficDelta{{TrafficID: 3, Email: "a", Up: math.MaxInt64 - 1, Down: 5}})
	m.RequeueClientTraffic([]ClientTrafficDelta{{TrafficID: 3, Email: "a", Up: 100, Down: -1}})
	_, got := m.CollectAllTraffic()
	if len(got) != 1 || got[0].Up != math.MaxInt64 || got[0].Down != -1 {
		t.Fatalf("requeued counters wrapped or discarded invalid input: %+v", got)
	}
}

func TestManagerRequeuesClientTrafficWithoutLosingDeltas(t *testing.T) {
	m := &Manager{servers: make(map[int]*managed)}
	m.RequeueClientTraffic([]ClientTrafficDelta{{Email: "shared@example.test", Up: 10, Down: 20}})
	m.RequeueClientTraffic([]ClientTrafficDelta{{Email: "shared@example.test", Up: 30, Down: 40}})

	_, got := m.CollectAllTraffic()
	if len(got) != 1 || got[0] != (ClientTrafficDelta{Email: "shared@example.test", Up: 40, Down: 60}) {
		t.Fatalf("requeued client traffic = %+v", got)
	}
	if _, got = m.CollectAllTraffic(); len(got) != 0 {
		t.Fatalf("requeued traffic was collected more than once: %+v", got)
	}
}
