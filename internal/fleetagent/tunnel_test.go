package fleetagent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

const cloudflaredTestTunnelID = "11111111-1111-4111-8111-111111111111"

func cloudflaredTestConfig() json.RawMessage {
	return json.RawMessage(`{
		"tunnelId":"11111111-1111-4111-8111-111111111111",
		"credentials":{
			"AccountTag":"account-tag",
			"TunnelSecret":"secret-material",
			"TunnelID":"11111111-1111-4111-8111-111111111111"
		}
	}`)
}

func TestCloudflaredProviderConfigRequiresMatchingCredentialIdentity(t *testing.T) {
	config, err := parseCloudflaredProviderConfig(cloudflaredTestConfig())
	if err != nil || config.TunnelID != cloudflaredTestTunnelID {
		t.Fatalf("valid cloudflared config = (%+v, %v)", config, err)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"tunnelId":"not-a-uuid","credentials":{}}`),
		json.RawMessage(`{"tunnelId":"11111111-1111-4111-8111-111111111111","credentials":{"AccountTag":"a","TunnelSecret":"s","TunnelID":"22222222-2222-4222-8222-222222222222"}}`),
		json.RawMessage(`{"tunnelId":"11111111-1111-4111-8111-111111111111","credentials":{},"command":"sh"}`),
		json.RawMessage(`{"tunnelId":"11111111-1111-4111-8111-111111111111","credentials":{"AccountTag":"a","TunnelSecret":"s","TunnelID":"11111111-1111-4111-8111-111111111111"}} {}`),
	} {
		if _, err := parseCloudflaredProviderConfig(raw); err == nil {
			t.Fatalf("unsafe cloudflared config accepted: %s", raw)
		}
	}
}

func TestTunnelManagerReportsUnsupportedWithoutConfiguredExecutable(t *testing.T) {
	manager, err := newTunnelManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	tunnel := service.AgentTunnelDelivery{
		ID: 1, Name: "app", Provider: "cloudflared", PublicHostname: "app.example.com",
		TargetHost: "127.0.0.1", TargetPort: 3000, Protocol: "http", ProviderConfig: cloudflaredTestConfig(),
	}
	if err := manager.Reconcile([]service.AgentTunnelDelivery{tunnel}); err == nil {
		t.Fatal("cloudflared tunnel started without a locally configured executable")
	}
	reports := manager.Reports()
	if len(reports) != 1 || reports[0].Status != "unsupported" || reports[0].TunnelID != 1 {
		t.Fatalf("unexpected unsupported report: %+v", reports)
	}
}

func TestTunnelManagerStartsFixedBinaryWithFixedArgumentsAndCleansRevokedFiles(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "arguments")
	binaryPath := filepath.Join(dir, "cloudflared-test")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TUNNEL_ARGS_OUTPUT\"\nsleep 30 &\nchild=$!\ntrap 'kill \"$child\" 2>/dev/null; exit 0' INT TERM\nwait \"$child\"\n"
	if err := os.WriteFile(binaryPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TUNNEL_ARGS_OUTPUT", argsPath)
	tunnelDir := filepath.Join(dir, "tunnels")
	manager, err := newTunnelManager(tunnelDir, binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(tunnelDir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("managed tunnel directory protection = (%v, %v), want 0700", info, err)
	}
	t.Cleanup(manager.Close)
	tunnel := service.AgentTunnelDelivery{
		ID: 7, Name: "app", Provider: "cloudflared", PublicHostname: "app.example.com",
		TargetHost: "::1", TargetPort: 3000, Protocol: "http", ProviderConfig: cloudflaredTestConfig(),
	}
	if err := manager.Reconcile([]service.AgentTunnelDelivery{tunnel}); err != nil {
		t.Fatal(err)
	}
	args := waitForTunnelFile(t, argsPath)
	configPath := filepath.Join(dir, "tunnels", "tunnel-7", "config.yml")
	if got, want := strings.Split(strings.TrimSpace(string(args)), "\n"), []string{"--no-autoupdate", "--config", configPath, "tunnel", "run"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("connector args = %#v, want %#v", got, want)
	}
	for _, path := range []string{configPath, filepath.Join(dir, "tunnels", "tunnel-7", "credentials.json")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("private tunnel file permissions for %s = %o, want 0600", path, info.Mode().Perm())
		}
	}
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "service: 'http://[::1]:3000'") || strings.Contains(string(config), "secret-material") {
		t.Fatalf("unexpected generated cloudflared config: %s", config)
	}
	if err := manager.Reconcile(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tunnels", "tunnel-7")); !os.IsNotExist(err) {
		t.Fatalf("revoked tunnel credentials were not removed: %v", err)
	}
}

func TestTunnelFilesAreOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential.json")
	if err := writePrivateFile(path, []byte(`{"credential":"private"}`)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private tunnel file permissions = %o, want 0600", info.Mode().Perm())
	}
}

func waitForTunnelFile(t *testing.T, path string) []byte {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if body, err := os.ReadFile(path); err == nil {
			return body
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for connector output %s", path)
	return nil
}
