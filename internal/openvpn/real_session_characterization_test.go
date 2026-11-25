package openvpn

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This characterizes the current lack of revocation; it is NOT a security
// guarantee. It runs only in a dedicated root-owned Linux network namespace.
func TestRealOpenVPNPreviouslyIssuedProfileStillConnects(t *testing.T) {
	binary := os.Getenv("PHOENIX_TEST_OPENVPN_LIVE_BINARY")
	if binary == "" {
		t.Skip("PHOENIX_TEST_OPENVPN_LIVE_BINARY not configured")
	}
	serverNS, clientNS := setupOpenVPNLiveNamespaces(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	inst := Instance{Id: 119, Port: 31194, Listen: "192.0.2.1", Proto: "udp"}
	profile, err := BuildProfile(inst, "alice@example.test", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	// Reconcile without Alice: deleting local material is not revocation.
	if err := pruneClientCerts(dataDirForID(inst.Id), nil); err != nil {
		t.Fatal(err)
	}
	if crt, _ := clientFiles(dataDirForID(inst.Id), "alice@example.test"); fileExists(crt) {
		t.Fatal("fixture did not remove Alice's local certificate")
	}
	if err := writeConfig(inst, 31195); err != nil {
		t.Fatal(err)
	}
	stripProductionHooks(t, inst)
	start := func(ns, config string) {
		t.Helper()
		cmd := exec.Command("ip", "netns", "exec", ns, binary, "--config", config)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start OpenVPN: %v", err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	}
	waitFor := func(path, marker string) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), marker) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		data, _ := os.ReadFile(path)
		serverLog, _ := os.ReadFile(logPathForID(inst.Id))
		t.Fatalf("missing %q in %s:\n%s\nserver log:\n%s", marker, path, data, serverLog)
	}
	start(serverNS, confPathForID(inst.Id))
	waitFor(logPathForID(inst.Id), "Initialization Sequence Completed")
	clientLog := filepath.Join(t.TempDir(), "client.log")
	clientConf := filepath.Join(t.TempDir(), "client.ovpn")
	profile += "\nlog " + quoteConfigPath(clientLog) + "\n"
	if err := os.WriteFile(clientConf, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	start(clientNS, clientConf)
	waitFor(clientLog, "Initialization Sequence Completed")
}

// The two OpenVPN peers communicate only through an isolated veth pair.
func setupOpenVPNLiveNamespaces(t *testing.T) (string, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatal("live OpenVPN characterization requires root")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Fatalf("ip tool required: %v", err)
	}
	serverNS := fmt.Sprintf("phoenix-ovpn-s-%d", os.Getpid())
	clientNS := fmt.Sprintf("phoenix-ovpn-c-%d", os.Getpid())
	ip := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v\n%s", args, err, out)
		}
	}
	for _, ns := range []string{serverNS, clientNS} {
		ip("netns", "add", ns)
		name := ns
		t.Cleanup(func() { _ = exec.Command("ip", "netns", "delete", name).Run() })
		ip("-n", ns, "link", "set", "lo", "up")
	}
	// Create the veth in the server namespace; neither endpoint is ever
	// attached to the host network stack.
	ip("-n", serverNS, "link", "add", "ovpn-s", "type", "veth", "peer", "name", "ovpn-c")
	ip("-n", serverNS, "link", "set", "ovpn-c", "netns", clientNS)
	ip("-n", serverNS, "addr", "add", "192.0.2.1/30", "dev", "ovpn-s")
	ip("-n", clientNS, "addr", "add", "192.0.2.2/30", "dev", "ovpn-c")
	ip("-n", serverNS, "link", "set", "ovpn-s", "up")
	ip("-n", clientNS, "link", "set", "ovpn-c", "up")
	return serverNS, clientNS
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// stripProductionHooks removes the panel-managed authorization/evidence
// directives from a live test configuration. Live characterization tests use
// their own temporary hooks and must not accidentally exercise or be blocked
// by the production allowlist. Production configs always keep these lines.
func stripProductionHooks(t *testing.T, inst Instance) {
	t.Helper()
	data, err := os.ReadFile(confPathForID(inst.Id))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(line, "script-security ") || strings.HasPrefix(line, "tls-verify ") ||
			strings.HasPrefix(line, "client-connect ") || strings.HasPrefix(line, "client-disconnect ") {
			continue
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(confPathForID(inst.Id), []byte(strings.Join(kept, "\n")), 0o640); err != nil {
		t.Fatal(err)
	}
}
