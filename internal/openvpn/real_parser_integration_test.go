package openvpn

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This checks parsing and path decoding using a real OpenVPN binary. It does not
// create a VPN session, test admission, prove termination or account traffic.
func TestRealOpenVPNConfigParser(t *testing.T) {
	binary := os.Getenv("PHOENIX_TEST_OPENVPN_BINARY")
	if binary == "" {
		t.Skip("PHOENIX_TEST_OPENVPN_BINARY not configured")
	}
	root := filepath.Join(t.TempDir(), `folder with "quotes" and \backslash`)
	t.Setenv("XUI_BIN_FOLDER", root)
	inst := Instance{Id: 118, Port: 1194, Listen: "127.0.0.1", PushDNS: true}
	profile, err := BuildProfile(inst, "alice@example.test", "vpn.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(inst, 41342); err != nil {
		t.Fatal(err)
	}
	server, err := os.ReadFile(confPathForID(inst.Id))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(t.TempDir(), "client.ovpn")
	if err := os.WriteFile(profilePath, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{confPathForID(inst.Id), profilePath} {
		cmd := exec.Command(binary, "--config", path, "--show-ciphers")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("parse %s: %v\n%s", path, err, out)
		}
	}
	// Prove that the parser invocation really inspects options rather than
	// exiting early before it has read the whole configuration.
	bad := filepath.Join(t.TempDir(), "invalid.conf")
	if err := os.WriteFile(bad, append([]byte(profile), []byte("\nphoenix-nonexistent-option value\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(binary, "--config", bad, "--show-ciphers").CombinedOutput(); err == nil {
		t.Fatalf("parser ignored invalid option: %s", out)
	}

	// Avoid writing daemon logs or PID files; the impossible dev-node stops
	// startup after option postprocessing, without requiring a tunnel device.
	var lines []string
	for _, line := range strings.Split(string(server), "\n") {
		if !strings.HasPrefix(line, "log ") && !strings.HasPrefix(line, "writepid ") {
			lines = append(lines, line)
		}
	}
	config := filepath.Join(t.TempDir(), "server-check.conf")
	if err := os.WriteFile(config, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--config", config, "--verb", "4", "--dev-node", filepath.Join(t.TempDir(), "missing-tun"))
	out, _ := cmd.CombinedOutput()
	for _, pair := range []struct{ name, file string }{
		{"ca_file", "ca.crt"}, {"cert_file", "server.crt"}, {"priv_key_file", "server.key"},
	} {
		want := fmt.Sprintf("%s = '%s'", pair.name, filepath.Join(dataDirForID(inst.Id), pair.file))
		if !strings.Contains(string(out), want) {
			t.Fatalf("real parser did not decode %s: expected %q\n%s", pair.name, want, out)
		}
	}
}
