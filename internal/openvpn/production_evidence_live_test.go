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

// This live test exercises the production hooks and generated config, not a
// test-only disconnect script. It runs only when a real OpenVPN binary and
// isolated root-owned network namespaces are available (see CI).
func TestRealOpenVPNProductionSessionEvidence(t *testing.T) {
	binary := os.Getenv("PHOENIX_TEST_OPENVPN_LIVE_BINARY")
	if binary == "" {
		t.Skip("PHOENIX_TEST_OPENVPN_LIVE_BINARY not configured")
	}
	serverNS, clientNS := setupOpenVPNLiveNamespaces(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())

	const email = "evidence@example.test"
	inst := Instance{Id: 122, Port: 31594, Listen: "192.0.2.1", Proto: "udp", Clients: []string{email}}
	profile, err := BuildProfile(inst, email, "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	dir := dataDirForID(inst.Id)
	defer func() {
		for _, sub := range []string{evidenceInc, evidenceRecords} {
			files, _ := os.ReadDir(filepath.Join(dir, evidenceRoot, sub))
			for _, file := range files {
				data, _ := os.ReadFile(filepath.Join(dir, evidenceRoot, sub, file.Name()))
				t.Logf("session evidence %s/%s: %s", sub, file.Name(), data)
			}
		}
		log, _ := os.ReadFile(logPathForID(inst.Id))
		if len(log) > 900 {
			log = log[len(log)-900:]
		}
		t.Logf("server log tail: %s", log)
	}()
	generation, err := loadOrCreateDaemonGeneration(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(inst, 31595); err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(confPathForID(inst.Id))
	if err != nil {
		t.Fatal(err)
	}
	for _, directive := range []string{"script-security 2", "tls-verify ", "client-connect ", "client-disconnect "} {
		if !strings.Contains(string(conf), directive) {
			t.Fatalf("production config missing %q:\n%s", directive, conf)
		}
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

	first := runProductionClientSession(t, binary, clientNS, inst, profile, email, generation)
	second := runProductionClientSession(t, binary, clientNS, inst, profile, email, generation)
	if first == second {
		t.Fatalf("reconnect reused the durable session identity: %s", first)
	}

}

// runProductionClientSession connects one client through the generated config,
// produces traffic, closes it cleanly and acknowledges the resulting durable
// evidence. It returns the session identity so a reconnect can be compared.
func runProductionClientSession(t *testing.T, binary, ns string, inst Instance, profile, email, generation string) string {
	t.Helper()
	clientLog := filepath.Join(t.TempDir(), "client.log")
	clientConf := filepath.Join(t.TempDir(), "client.ovpn")
	body := profile + clientDataChannelDirectives(binary) + "explicit-exit-notify 1\nlog " + quoteConfigPath(clientLog) + "\n"
	if err := os.WriteFile(clientConf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	client := exec.Command("ip", "netns", "exec", ns, binary, "--config", clientConf)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if client.ProcessState == nil {
			_ = client.Process.Kill()
			_ = client.Wait()
		}
	})
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(clientLog)
		if strings.Contains(string(data), "Initialization Sequence Completed") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if data, _ := os.ReadFile(clientLog); !strings.Contains(string(data), "Initialization Sequence Completed") {
		server, _ := os.ReadFile(logPathForID(inst.Id))
		t.Fatalf("client did not initialize:\n%s\nserver:\n%s", data, server)
	}
	gateway := strings.TrimSuffix(inst.serverSubnetCIDR(), "0/24") + "1"
	out, err := exec.Command("ip", "netns", "exec", ns, "ping", "-n", "-c", "4", "-W", "3", gateway).CombinedOutput()
	if err != nil {
		t.Fatalf("tunnel did not carry ICMP: %v\n%s", err, out)
	}
	_ = client.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- client.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("client did not stop")
	}

	deadline = time.Now().Add(25 * time.Second)
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
		t.Fatalf("expected one durable session, got %d", len(sessions))
	}
	sample := sessions[0]
	if sample.Email != email || sample.InboundID != inst.Id || !strings.Contains(sample.Key, generation) {
		t.Fatalf("unexpected evidence identity: %+v (generation %s)", sample, generation)
	}
	if sample.Rx <= 0 || sample.Tx <= 0 {
		t.Fatalf("final counters missing bidirectional traffic: %+v", sample)
	}
	if err := AckSessionEvidence(sample); err != nil {
		t.Fatal(err)
	}
	if remaining, err := CollectSessionEvidence(); err != nil || len(remaining) != 0 {
		t.Fatalf("acknowledged evidence remained: %v %v", remaining, err)
	}
	return sample.Key
}

// clientDataChannelDirectives keeps the client userspace for versions that
// support DCO. OpenVPN 2.4 predates --disable-dco and fails config parsing if
// the directive is present, so it uses its normal userspace data channel.
func clientDataChannelDirectives(binary string) string {
	out, err := exec.Command(binary, "--version").CombinedOutput()
	if err != nil || strings.Contains(string(out), "OpenVPN 2.4.") {
		return "\n"
	}
	return "\ndisable-dco\n"
}
