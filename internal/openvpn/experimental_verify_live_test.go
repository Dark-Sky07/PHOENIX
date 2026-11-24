package openvpn

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This is a test-only TLS admission prototype, not production revocation.
// It is not used in generated configs or installed by PHOENIX.
func TestRealOpenVPNExperimentalFingerprintAdmission(t *testing.T) {
	binary := os.Getenv("PHOENIX_TEST_OPENVPN_LIVE_BINARY")
	if binary == "" {
		t.Skip("PHOENIX_TEST_OPENVPN_LIVE_BINARY not configured")
	}
	serverNS, clientNS := setupOpenVPNLiveNamespaces(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	inst := Instance{Id: 120, Port: 31294, Listen: "192.0.2.1", Proto: "udp"}
	alice, err := BuildProfile(inst, "alice@example.test", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := BuildProfile(inst, "bob@example.test", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	bobCert, bobKey := clientFiles(dataDirForID(inst.Id), "bob@example.test")
	cert, _, err := loadKeyPair(bobCert, bobKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(cert.Raw)
	hexDigest := hex.EncodeToString(digest[:])
	parts := make([]string, 0, 32)
	for i := 0; i < len(hexDigest); i += 2 {
		parts = append(parts, hexDigest[i:i+2])
	}
	dir := t.TempDir()
	allowlist := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(allowlist, []byte(strings.Join(parts, ":")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	verifier := filepath.Join(dir, "verify")
	script := "#!/bin/sh\n[ \"$1\" = 0 ] || exit 0\n[ -n \"${tls_digest_sha256_0:-}\" ] || exit 1\nexec grep -Fqx -- \"$tls_digest_sha256_0\" " + strconv.Quote(allowlist) + "\n"
	if err := os.WriteFile(verifier, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(inst, 31295); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(confPathForID(inst.Id), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(file, "script-security 2\ntls-verify %s\n", verifier)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("write verifier config: %v %v", err, closeErr)
	}
	start := func(ns, config string) {
		t.Helper()
		cmd := exec.Command("ip", "netns", "exec", ns, binary, "--config", config)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	}
	waitLog := func(path string, predicate func(string) bool) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(path)
			if predicate(string(data)) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		data, _ := os.ReadFile(path)
		server, _ := os.ReadFile(logPathForID(inst.Id))
		t.Fatalf("expected log event missing in %s:\n%s\nserver:\n%s", path, data, server)
	}
	client := func(name, text string) string {
		t.Helper()
		log := filepath.Join(t.TempDir(), name+".log")
		conf := filepath.Join(t.TempDir(), name+".ovpn")
		if err := os.WriteFile(conf, []byte(text+"\nlog "+quoteConfigPath(log)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		start(clientNS, conf)
		return log
	}
	start(serverNS, confPathForID(inst.Id))
	waitLog(logPathForID(inst.Id), func(s string) bool { return strings.Contains(s, "Initialization Sequence Completed") })
	bobLog := client("bob", bob)
	waitLog(bobLog, func(s string) bool { return strings.Contains(s, "Initialization Sequence Completed") })
	pingThroughBob := func() {
		t.Helper()
		out, err := exec.Command("ip", "netns", "exec", clientNS, "ping", "-n", "-c", "2", "-W", "3", "10.120.0.1").CombinedOutput()
		if err != nil {
			t.Fatalf("Bob's established tunnel did not carry ICMP: %v\n%s", err, out)
		}
	}
	pingThroughBob()
	client("alice", alice)
	denied := func(identity string) func(string) bool {
		return func(s string) bool {
			for _, line := range strings.Split(s, "\n") {
				if strings.Contains(line, "VERIFY SCRIPT ERROR: depth=0") && strings.Contains(line, identity) {
					return true
				}
			}
			return false
		}
	}
	waitLog(logPathForID(inst.Id), denied("alice@example.test"))
	// Missing allowlist must fail closed for a fresh authentication, too.
	if err := os.Remove(allowlist); err != nil {
		t.Fatal(err)
	}
	// Existing sessions are not reverified on every packet. This deliberately
	// characterizes continued forwarding, not an active-session revocation.
	pingThroughBob()
	client("bob-without-allowlist", bob)
	waitLog(logPathForID(inst.Id), denied("bob@example.test"))
	pingThroughBob()
}
