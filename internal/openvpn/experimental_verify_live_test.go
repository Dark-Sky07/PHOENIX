package openvpn

import (
	"context"
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
	carolNS := fmt.Sprintf("phoenix-ovpn-c2-%d", os.Getpid())
	ip := func(args ...string) {
		t.Helper()
		out, err := exec.Command("ip", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("ip %v: %v\n%s", args, err, out)
		}
	}
	ip("netns", "add", carolNS)
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "delete", carolNS).Run() })
	ip("-n", carolNS, "link", "set", "lo", "up")
	ip("-n", serverNS, "link", "add", "ovpn-s2", "type", "veth", "peer", "name", "ovpn-c2")
	ip("-n", serverNS, "link", "set", "ovpn-c2", "netns", carolNS)
	ip("-n", serverNS, "addr", "add", "192.0.2.5/30", "dev", "ovpn-s2")
	ip("-n", carolNS, "addr", "add", "192.0.2.6/30", "dev", "ovpn-c2")
	ip("-n", serverNS, "link", "set", "ovpn-s2", "up")
	ip("-n", carolNS, "link", "set", "ovpn-c2", "up")
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	inst := Instance{Id: 120, Port: 31294, Proto: "udp"}
	alice, err := BuildProfile(inst, "alice@example.test", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := BuildProfile(inst, "bob@example.test", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := BuildProfile(inst, "carol@example.test", "192.0.2.5")
	if err != nil {
		t.Fatal(err)
	}
	var fingerprints []string
	for _, email := range []string{"bob@example.test", "carol@example.test"} {
		crt, key := clientFiles(dataDirForID(inst.Id), email)
		cert, _, err := loadKeyPair(crt, key)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(cert.Raw)
		hexDigest := hex.EncodeToString(digest[:])
		parts := make([]string, 0, 32)
		for i := 0; i < len(hexDigest); i += 2 {
			parts = append(parts, hexDigest[i:i+2])
		}
		fingerprints = append(fingerprints, strings.Join(parts, ":"))
	}
	dir := t.TempDir()
	allowlist := filepath.Join(dir, "allowlist")
	if err := os.WriteFile(allowlist, []byte(strings.Join(fingerprints, "\n")+"\n"), 0600); err != nil {
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
	// Test-only hook: record exactly what OpenVPN provides when this session
	// disconnects. This plain file is not durable production accounting.
	disconnectLog := filepath.Join(dir, "disconnect-record")
	disconnectHook := filepath.Join(dir, "disconnect")
	hook := "#!/bin/sh\nprintf '%s\\t%s\\t%s\\n' \"${common_name:-}\" \"${bytes_received:-}\" \"${bytes_sent:-}\" >> " + strconv.Quote(disconnectLog) + "\n"
	if err := os.WriteFile(disconnectHook, []byte(hook), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(confPathForID(inst.Id), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(file, "script-security 2\ntls-verify %s\nclient-disconnect %s\n", verifier, disconnectHook)
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
	client := func(ns, name, text string) string {
		t.Helper()
		log := filepath.Join(t.TempDir(), name+".log")
		conf := filepath.Join(t.TempDir(), name+".ovpn")
		if err := os.WriteFile(conf, []byte(text+"\nlog "+quoteConfigPath(log)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		start(ns, conf)
		return log
	}
	start(serverNS, confPathForID(inst.Id))
	waitLog(logPathForID(inst.Id), func(s string) bool { return strings.Contains(s, "Initialization Sequence Completed") })
	bobLog := client(clientNS, "bob", bob)
	waitLog(bobLog, func(s string) bool { return strings.Contains(s, "Initialization Sequence Completed") })
	carolLog := client(carolNS, "carol", carol)
	waitLog(carolLog, func(s string) bool { return strings.Contains(s, "Initialization Sequence Completed") })
	pingThroughBob := func() {
		t.Helper()
		out, err := exec.Command("ip", "netns", "exec", clientNS, "ping", "-n", "-c", "2", "-W", "3", "10.120.0.1").CombinedOutput()
		if err != nil {
			t.Fatalf("Bob's established tunnel did not carry ICMP: %v\n%s", err, out)
		}
	}
	pingThroughBob()
	pingThroughCarol := func() {
		t.Helper()
		out, err := exec.Command("ip", "netns", "exec", carolNS, "ping", "-n", "-c", "2", "-W", "3", "10.120.0.1").CombinedOutput()
		if err != nil {
			t.Fatalf("Carol's tunnel did not carry ICMP: %v\n%s", err, out)
		}
	}
	pingThroughCarol()
	client(clientNS, "alice", alice)
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
	client(clientNS, "bob-without-allowlist", bob)
	waitLog(logPathForID(inst.Id), denied("bob@example.test"))
	pingThroughBob()

	// In the isolated experiment only, ask the real management socket for
	// the current session ID and kill that session. Never use a CN as a raw
	// management command; this test uses a parsed numeric CID.
	management := func(command string) string {
		t.Helper()
		const code = `import socket, sys
s = socket.create_connection(('127.0.0.1', 31295), timeout=4)
s.settimeout(4)
s.sendall((sys.argv[1] + '\n').encode())
buf = b''
while True:
    part = s.recv(65536)
    if not part:
        break
    buf += part
    if any(x.rstrip(b'\r') == b'END' or x.startswith((b'SUCCESS:', b'ERROR:')) for x in buf.split(b'\n')[:-1]):
        break
sys.stdout.buffer.write(buf)
`
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ip", "netns", "exec", serverNS, "python3", "-c", code, command).CombinedOutput()
		if err != nil {
			t.Fatalf("management %q: %v\n%s", command, err, out)
		}
		return strings.ReplaceAll(string(out), "\r\n", "\n")
	}
	status := management("status 3")
	clientIDIndex, rxIndex, txIndex := -1, -1, -1
	clientID := ""
	var observedRx, observedTx uint64
	for _, line := range strings.Split(status, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) > 2 && fields[0] == "HEADER" && fields[1] == "CLIENT_LIST" {
			for i, field := range fields {
				switch field {
				case "Client ID":
					clientIDIndex = i - 1
				case "Bytes Received":
					rxIndex = i - 1
				case "Bytes Sent":
					txIndex = i - 1
				}
			}
		}
		if len(fields) > 1 && fields[0] == "CLIENT_LIST" && fields[1] == "bob@example.test" {
			if clientID != "" || clientIDIndex < 0 || clientIDIndex >= len(fields) || rxIndex < 0 || rxIndex >= len(fields) || txIndex < 0 || txIndex >= len(fields) {
				t.Fatalf("ambiguous Bob session: %s", status)
			}
			clientID = fields[clientIDIndex]
			var err error
			observedRx, err = strconv.ParseUint(fields[rxIndex], 10, 64)
			if err != nil {
				t.Fatalf("invalid received counter: %s", status)
			}
			observedTx, err = strconv.ParseUint(fields[txIndex], 10, 64)
			if err != nil {
				t.Fatalf("invalid sent counter: %s", status)
			}
		}
	}
	if _, err := strconv.ParseUint(clientID, 10, 64); err != nil {
		t.Fatalf("missing or invalid Bob client ID: %q\n%s", clientID, status)
	}
	// Produce known traffic after the last management snapshot.
	pingThroughBob()
	if result := management("client-kill " + clientID); !strings.Contains(result, "SUCCESS: client-kill command succeeded") {
		t.Fatalf("management did not acknowledge kill: %s", result)
	}
	// A successful response is only an acknowledgement: the daemon can
	// still report the session briefly. Require a later complete status that
	// no longer contains it before testing the data path.
	deadline := time.Now().Add(10 * time.Second)
	for {
		result := management("status 3")
		if !strings.Contains(result, "HEADER\tCLIENT_LIST\t") || !strings.Contains(result, "\nEND\n") {
			t.Fatalf("incomplete status after kill: %s", result)
		}
		if !strings.Contains(result, "CLIENT_LIST\tbob@example.test\t") {
			if !strings.Contains(result, "CLIENT_LIST\tcarol@example.test\t") {
				t.Fatalf("Carol disappeared from status after Bob's kill: %s", result)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Bob not absent within observation window: %s", result)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The disconnect hook should see the extra traffic in this one live
	// run. It cannot recover a hook that never executes or data after a crash.
	waitLog(disconnectLog, func(text string) bool { return strings.Contains(text, "bob@example.test\t") })
	data, err := os.ReadFile(disconnectLog)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] != "bob@example.test" {
			continue
		}
		if found {
			t.Fatalf("duplicate Bob disconnect record: %s", data)
		}
		found = true
		rx, errRx := strconv.ParseUint(fields[1], 10, 64)
		tx, errTx := strconv.ParseUint(fields[2], 10, 64)
		if errRx != nil || errTx != nil || rx <= observedRx || tx <= observedTx {
			t.Fatalf("disconnect counters did not include post-snapshot traffic: %q (snapshot %d/%d)", line, observedRx, observedTx)
		}
	}
	if !found {
		t.Fatalf("missing Bob disconnect record: %s", data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", "netns", "exec", clientNS, "ping", "-n", "-c", "1", "-W", "2", "10.120.0.1").CombinedOutput()
	if err == nil {
		t.Fatalf("Bob still received ICMP after management kill: %s", out)
	}
	pingThroughCarol()
}
