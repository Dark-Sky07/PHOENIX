package openvpn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This live test keeps DCO enabled on both peers and verifies that the
// production hooks still receive final counters. It skips when the kernel or
// the tested OpenVPN build does not actually open a DCO data channel; a skip
// is not evidence that DCO is safe.
func TestRealOpenVPNProductionDCOEvidence(t *testing.T) {
	binary := os.Getenv("PHOENIX_TEST_OPENVPN_LIVE_BINARY")
	if binary == "" {
		t.Skip("PHOENIX_TEST_OPENVPN_LIVE_BINARY not configured")
	}
	version, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil && !strings.Contains(string(version), "OpenVPN") {
		t.Fatalf("read OpenVPN version: %v\n%s", err, version)
	}
	if strings.Contains(string(version), "OpenVPN 2.4.") {
		t.Skip("OpenVPN 2.4 has no DCO data channel")
	}
	serverNS, clientNS := setupOpenVPNLiveNamespaces(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())

	const email = "dco@example.test"
	const inboundID = 125
	const mgmtPort = 31895
	inst := Instance{Id: inboundID, Port: 31894, Listen: "192.0.2.1", Proto: "udp", Clients: []string{email}}
	profile, err := BuildProfile(inst, email, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	dir := dataDirForID(inst.Id)
	generation, err := loadOrCreateDaemonGeneration(dir)
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("missing %q in %s:\n%s", marker, path, data)
	}

	server := start(serverNS, confPathForID(inst.Id))
	defer func() { _ = server.Process.Kill(); _ = server.Wait() }()
	waitFor(logPathForID(inst.Id), "Initialization Sequence Completed", 25*time.Second)
	serverLog, err := os.ReadFile(logPathForID(inst.Id))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(serverLog), "DCO device") && !strings.Contains(string(serverLog), "ovpn-dco device") {
		t.Skip("server did not open an ovpn-dco device on this kernel")
	}

	clientLog := filepath.Join(t.TempDir(), "client.log")
	clientConf := filepath.Join(t.TempDir(), "client.ovpn")
	if err := os.WriteFile(clientConf, []byte(profile+"explicit-exit-notify 1\nlog "+quoteConfigPath(clientLog)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := start(clientNS, clientConf)
	defer func() {
		if client.ProcessState == nil {
			_ = client.Process.Kill()
			_ = client.Wait()
		}
	}()
	waitFor(clientLog, "Initialization Sequence Completed", 25*time.Second)
	data, err := os.ReadFile(clientLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "DCO device") && !strings.Contains(string(data), "ovpn-dco device") {
		t.Skip("client did not open an ovpn-dco device on this kernel")
	}
	out, err := exec.Command("ip", "netns", "exec", clientNS, "ping", "-n", "-c", "6", "-W", "3", "10.125.0.1").CombinedOutput()
	if err != nil {
		t.Fatalf("DCO tunnel did not carry ICMP: %v\n%s\nclient:\n%s", err, out, data)
	}
	_ = client.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("DCO client did not stop")
	}

	deadline := time.Now().Add(25 * time.Second)
	var sessions []SessionEvidence
	for time.Now().Before(deadline) {
		sessions, err = CollectSessionEvidence()
		if err == nil && len(sessions) == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected one durable DCO session, got %d", len(sessions))
	}
	sample := sessions[0]
	if sample.Email != email || sample.InboundID != inboundID || !strings.Contains(sample.Key, generation) {
		t.Fatalf("unexpected DCO evidence identity: %+v", sample)
	}
	if sample.Rx <= 0 || sample.Tx <= 0 {
		t.Fatalf("DCO final counters missing bidirectional traffic: %+v", sample)
	}
	if err := AckSessionEvidence(sample); err != nil {
		t.Fatal(err)
	}
}
