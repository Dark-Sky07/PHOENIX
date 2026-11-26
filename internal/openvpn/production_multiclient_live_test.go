package openvpn

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Two clients with distinct identities and source addresses admit and close
// concurrently through the generated production hooks. This proves per-client
// final evidence for one OpenVPN package; it is not a multi-node or DCO test.
func TestRealOpenVPNProductionConcurrentAdmissionEvidence(t *testing.T) {
	binary := os.Getenv("PHOENIX_TEST_OPENVPN_LIVE_BINARY")
	if binary == "" {
		t.Skip("PHOENIX_TEST_OPENVPN_LIVE_BINARY not configured")
	}
	serverNS, clientANS := setupOpenVPNLiveNamespaces(t)
	clientBNS := fmt.Sprintf("phoenix-ovpn-b-%d", os.Getpid())
	ip := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v\n%s", args, err, out)
		}
	}
	ip("netns", "add", clientBNS)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "delete", clientBNS).Run() })
	ip("-n", clientBNS, "link", "set", "lo", "up")
	ip("-n", serverNS, "link", "add", "ovpn-sb", "type", "veth", "peer", "name", "ovpn-cb")
	ip("-n", serverNS, "link", "set", "ovpn-cb", "netns", clientBNS)
	ip("-n", serverNS, "addr", "add", "192.0.2.5/30", "dev", "ovpn-sb")
	ip("-n", clientBNS, "addr", "add", "192.0.2.6/30", "dev", "ovpn-cb")
	ip("-n", serverNS, "link", "set", "ovpn-sb", "up")
	ip("-n", clientBNS, "link", "set", "ovpn-cb", "up")

	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	const inboundID = 124
	const mgmtPort = 31795
	emails := []string{"multi-a@example.test", "multi-b@example.test"}
	hosts := []string{"192.0.2.1", "192.0.2.5"}
	namespaces := []string{clientANS, clientBNS}
	inst := Instance{Id: inboundID, Port: 31794, Proto: "udp", Clients: emails}
	profiles := make([]string, 0, len(emails))
	for i, email := range emails {
		profile, err := BuildProfile(inst, email, hosts[i])
		if err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, profile)
	}
	if err := writeConfig(inst, mgmtPort); err != nil {
		t.Fatal(err)
	}

	start := func(ns, config string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("ip", "netns", "exec", ns, binary, "--config", config)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	waitFor := func(path, marker string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), marker) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		data, _ := os.ReadFile(path)
		server, _ := os.ReadFile(logPathForID(inst.Id))
		t.Fatalf("missing %q in %s:\n%s\nserver:\n%s", marker, path, data, server)
	}

	server := start(serverNS, confPathForID(inst.Id))
	defer func() { _ = server.Process.Kill(); _ = server.Wait() }()
	waitFor(logPathForID(inst.Id), "Initialization Sequence Completed", 25*time.Second)

	clients := make([]*exec.Cmd, 0, 2)
	for i, email := range emails {
		profile := profiles[i]
		logPath := filepath.Join(t.TempDir(), fmt.Sprintf("client-%d.log", i))
		confPath := filepath.Join(t.TempDir(), fmt.Sprintf("client-%d.ovpn", i))
		body := profile + clientDataChannelDirectives(binary) + "explicit-exit-notify 1\nlog " + quoteConfigPath(logPath) + "\n"
		if err := os.WriteFile(confPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		clients = append(clients, start(namespaces[i], confPath))
		waitFor(logPath, "Initialization Sequence Completed", 25*time.Second)
		out, err := exec.Command("ip", "netns", "exec", namespaces[i], "ping", "-n", "-c", "3", "-W", "3", "10.124.0.1").CombinedOutput()
		if err != nil {
			t.Fatalf("client %s did not carry ICMP: %v\n%s", email, err, out)
		}
	}

	for _, client := range clients {
		_ = client.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func(c *exec.Cmd) { done <- c.Wait() }(client)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatal("client did not stop")
		}
	}

	deadline := time.Now().Add(25 * time.Second)
	var sessions []SessionEvidence
	for time.Now().Before(deadline) {
		var err error
		sessions, err = CollectSessionEvidence()
		if err == nil && len(sessions) == 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected two durable sessions, got %d", len(sessions))
	}
	seen := map[string]bool{}
	for _, sample := range sessions {
		if sample.InboundID != inboundID || sample.Rx <= 0 || sample.Tx <= 0 {
			t.Fatalf("unexpected final evidence: %+v", sample)
		}
		seen[sample.Email] = true
		if err := AckSessionEvidence(sample); err != nil {
			t.Fatal(err)
		}
	}
	for _, email := range emails {
		if !seen[email] {
			t.Fatalf("no final evidence for %s", email)
		}
	}
}
