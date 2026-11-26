package openvpn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRealOpenVPNRevocationHelper is re-executed inside the server network
// namespace by TestRealOpenVPNProductionRevocationTerminatesSession. The
// management socket is bound to that namespace's loopback, so the production
// RevokeClient path must run there to exercise a real client-kill.
func TestRealOpenVPNRevocationHelper(t *testing.T) {
	if os.Getenv("PHOENIX_REVOKE_HELPER") != "1" {
		t.Skip("namespace-only revocation helper")
	}
	inbound, err := strconv.Atoi(os.Getenv("PHOENIX_REVOKE_INBOUND"))
	if err != nil || inbound <= 0 {
		t.Fatalf("invalid inbound: %v", err)
	}
	port, err := strconv.Atoi(os.Getenv("PHOENIX_REVOKE_MGMT_PORT"))
	if err != nil || port <= 0 {
		t.Fatalf("invalid management port: %v", err)
	}
	email := os.Getenv("PHOENIX_REVOKE_EMAIL")
	generation := os.Getenv("PHOENIX_REVOKE_GENERATION")
	mgr := &Manager{procs: map[int]*managed{
		inbound: {generation: generation, mgmtPort: port, clients: []string{email}},
	}}
	if err := mgr.RevokeClient(email, generation); err != nil {
		t.Fatal(err)
	}
}

// This test drives the production revocation path against a real OpenVPN
// daemon, then verifies that the disconnect hook produced final evidence.
// It proves termination and evidence for one client on one OpenVPN package;
// it does not prove traffic quiescence or multi-client behavior.
func TestRealOpenVPNProductionRevocationTerminatesSession(t *testing.T) {
	binary := os.Getenv("PHOENIX_TEST_OPENVPN_LIVE_BINARY")
	if binary == "" {
		t.Skip("PHOENIX_TEST_OPENVPN_LIVE_BINARY not configured")
	}
	serverNS, clientNS := setupOpenVPNLiveNamespaces(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())

	const email = "revoke@example.test"
	const inboundID = 123
	const mgmtPort = 31695
	inst := Instance{Id: inboundID, Port: 31694, Listen: "192.0.2.1", Proto: "udp", Clients: []string{email}}
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
	digest, err := certificateDigestForClient(dir, email)
	if err != nil {
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

	clientLog := filepath.Join(t.TempDir(), "client.log")
	clientConf := filepath.Join(t.TempDir(), "client.ovpn")
	if err := os.WriteFile(clientConf, []byte(profile+clientDataChannelDirectives(binary)+"explicit-exit-notify 1\nlog "+quoteConfigPath(clientLog)+"\n"), 0o600); err != nil {
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
	probe, err := exec.Command("ip", "netns", "exec", clientNS, "ping", "-n", "-c", "4", "-W", "3", "10.123.0.1").CombinedOutput()
	if err != nil {
		t.Fatalf("tunnel did not carry ICMP: %v\n%s", err, probe)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command("ip", "netns", "exec", serverNS, self,
		"-test.run=^TestRealOpenVPNRevocationHelper$", "-test.v")
	helper.Env = append(os.Environ(),
		"PHOENIX_REVOKE_HELPER=1",
		"PHOENIX_REVOKE_INBOUND="+strconv.Itoa(inboundID),
		"PHOENIX_REVOKE_MGMT_PORT="+strconv.Itoa(mgmtPort),
		"PHOENIX_REVOKE_EMAIL="+email,
		"PHOENIX_REVOKE_GENERATION="+generation,
	)
	out, err := helper.CombinedOutput()
	if err != nil {
		t.Fatalf("production revocation failed: %v\n%s\nserver:\n%s", err, out, mustRead(logPathForID(inst.Id)))
	}
	// The client-kill may already have ended the process; a cleanup signal is
	// best-effort and the wait below is the authoritative stop observation.
	_ = client.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("client did not stop after revocation")
	}

	allow, err := os.ReadFile(filepath.Join(dir, "client-allowlist"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(allow), digest) {
		t.Fatal("revoked digest remained in the allowlist")
	}
	deny, err := os.ReadFile(filepath.Join(dir, "client-revocations"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(deny), digest) {
		t.Fatal("revoked digest was not persisted")
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
		t.Fatalf("expected one durable session after revocation, got %d", len(sessions))
	}
	sample := sessions[0]
	if sample.Email != email || sample.InboundID != inboundID || sample.Rx <= 0 || sample.Tx <= 0 {
		t.Fatalf("unexpected final evidence after revocation: %+v", sample)
	}
	if err := AckSessionEvidence(sample); err != nil {
		t.Fatal(err)
	}
}

func mustRead(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
