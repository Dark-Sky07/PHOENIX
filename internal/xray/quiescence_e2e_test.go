package xray

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
)

// This characterizes the pinned core, not a quiescence implementation. It is
// intentionally tied to a real VLESS TCP stream; no result here certifies the
// behavior of other protocols, binaries, remote hosts, or concurrent writers.
func TestXrayRemoveUserDoesNotDrainEstablishedVLESS(t *testing.T) {
	bin := os.Getenv("XRAY_E2E_BINARY")
	if bin == "" {
		t.Skip("set XRAY_E2E_BINARY to the pinned Xray binary")
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	apiPort, userPort := freePort(t), freePort(t)
	for userPort == apiPort {
		userPort = freePort(t)
	}
	userID := uuid.MustParse("a17e367c-2074-4d3e-aaeb-fbef5dfde7e7")
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"api": map[string]any{"services": []string{"HandlerService", "StatsService"}, "tag": "api"},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": apiPort, "protocol": "tunnel", "settings": map[string]any{"rewriteAddress": "127.0.0.1"}, "tag": "api"},
			map[string]any{"listen": "127.0.0.1", "port": userPort, "protocol": "vless", "settings": map[string]any{"clients": []any{map[string]any{"id": userID.String(), "email": "quiescence@example.test"}}, "decryption": "none"}, "tag": "user-in"},
		},
		"outbounds": []any{map[string]any{"protocol": "freedom", "settings": map[string]any{}, "tag": "direct"}},
		"routing":   map[string]any{"rules": []any{map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"}}},
		"policy":    map[string]any{"levels": map[string]any{"0": map[string]any{"statsUserOnline": true}}}, "stats": map[string]any{},
	}
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "xray.json")
	if err := os.WriteFile(config, body, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-c", config)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	waitForPort(t, apiPort)
	waitForPort(t, userPort)
	api := XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(userPort)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	echoPort := echo.Addr().(*net.TCPAddr).Port
	// VLESS: version, UUID, addon length, TCP command, port, IPv4 address.
	header := append([]byte{0}, userID[:]...)
	header = append(header, 0, 1, byte(echoPort>>8), byte(echoPort), 1, 127, 0, 0, 1)
	if _, err := conn.Write(append(header, []byte("before")...)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 2+len("before"))
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(response[:2]) != 0 || string(response[2:]) != "before" {
		t.Fatalf("unexpected VLESS response %x", response)
	}
	if err := api.RemoveUser("user-in", "quiescence@example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("after")); err != nil {
		t.Fatal(err)
	}
	after := make([]byte, len("after"))
	if _, err := io.ReadFull(conn, after); err != nil {
		t.Fatalf("pinned core changed existing-session behavior: %v", err)
	}
	if string(after) != "after" {
		t.Fatalf("unexpected response %q", after)
	}
}
