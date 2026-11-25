package openvpn

import (
	"bufio"
	"net"
	"strings"
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
