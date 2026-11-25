package openvpn

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func startFakeManagement(t *testing.T, reply string, captured *string) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err == nil && captured != nil {
			*captured = strings.TrimSpace(line)
		}
		_, _ = conn.Write([]byte(reply + "\r\n"))
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

func TestManagementKillRequiresNumericIDAndSuccess(t *testing.T) {
	var command string
	port := startFakeManagement(t, "SUCCESS: client-kill", &command)
	if err := managementKill(port, "42"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if command != "client-kill 42" {
		t.Fatalf("unexpected management command %q", command)
	}
	if err := managementKill(port, "alice@example.test"); err == nil {
		t.Fatal("client name accepted as a management command")
	}
	failPort := startFakeManagement(t, "ERROR: unknown client ID", nil)
	if err := managementKill(failPort, "7"); err == nil {
		t.Fatal("ERROR reply treated as success")
	}
}

func TestRevokeClientRejectsStaleGeneration(t *testing.T) {
	m := &Manager{procs: map[int]*managed{}}
	if err := m.RevokeClient("alice@example.test", strings.Repeat("a", 32)); err == nil {
		t.Fatal("revocation accepted without a managed daemon")
	}
	if err := m.RevokeClientEverywhere("alice@example.test"); err == nil {
		t.Fatal("revocation accepted without any managed daemon")
	}
}

func TestRevocationSurvivesAllowlistRegeneration(t *testing.T) {
	dir, _ := setupEvidenceTest(t, 746, "revoked@example.test")
	digest, err := certificateDigestForClient(dir, "revoked@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := recordRevokedDigest(dir, digest); err != nil {
		t.Fatal(err)
	}
	digests, err := loadRevokedDigests(dir)
	if err != nil || !digests[digest] {
		t.Fatalf("revocation not persisted: %v %v", digests, err)
	}
	// Simulate a daemon restart or re-attachment regenerating the allowlist.
	if err := writeClientAllowlist(dir, []string{"revoked@example.test"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "client-allowlist"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), digest) {
		t.Fatalf("revoked fingerprint re-authorized: %q", data)
	}
	if err := os.WriteFile(filepath.Join(dir, "client-revocations"), []byte("malformed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRevokedDigests(dir); err == nil {
		t.Fatal("malformed revocation state accepted")
	}
}

func TestRevokeClientPersistsDenyAndVerifiesTermination(t *testing.T) {
	dir, generation := setupEvidenceTest(t, 747, "alice@example.test")
	digest, err := certificateDigestForClient(dir, "alice@example.test")
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var mu sync.Mutex
	killed := false
	commands := make([]string, 0, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					command := strings.TrimSpace(line)
					mu.Lock()
					commands = append(commands, command)
					reply := tabHeader + "END\n"
					switch {
					case strings.HasPrefix(command, "status ") && !killed:
						reply = tabHeader + tabClient + "END\n"
					case strings.HasPrefix(command, "client-kill "):
						killed = true
						reply = "SUCCESS: client-kill accepted\r\n"
					}
					mu.Unlock()
					_, _ = conn.Write([]byte(reply))
				}
			}(conn)
		}
	}()

	mgr := &Manager{procs: map[int]*managed{
		747: {
			mgmtPort:   listener.Addr().(*net.TCPAddr).Port,
			clients:    []string{"alice@example.test"},
			generation: generation,
		},
	}}
	if err := mgr.RevokeClient("alice@example.test", generation); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	mu.Lock()
	gotCommands := append([]string(nil), commands...)
	mu.Unlock()
	if !slices.Contains(gotCommands, "client-kill 1") {
		t.Fatalf("numeric client ID was not killed: %v", gotCommands)
	}
	revoked, err := loadRevokedDigests(dir)
	if err != nil || !revoked[digest] {
		t.Fatalf("revocation state not persisted: %v %v", revoked, err)
	}
	allow, err := os.ReadFile(filepath.Join(dir, "client-allowlist"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(allow), digest) {
		t.Fatalf("revoked digest still authorized: %q", allow)
	}
}
