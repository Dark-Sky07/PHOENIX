package xray

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	"github.com/xtls/xray-core/common/buf"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/proxy/shadowsocks"
)

// Experimental legacy AEAD Shadowsocks UDP only. This is a bounded packet
// observation, not a counter flush, transport-wide proof, or quiescence fence.
func TestExperimentalShadowsocksUDPDisconnect(t *testing.T) {
	bin := os.Getenv("XRAY_E2E_BINARY")
	if bin == "" {
		t.Skip("set XRAY_E2E_BINARY to an Xray binary")
	}
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(b[:n], addr)
		}
	}()
	apiPort, userPort := freePort(t), freePort(t)
	if apiPort == userPort {
		t.Skip("ephemeral port collision")
	}
	echoPort := echo.LocalAddr().(*net.UDPAddr).Port
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"}, "api": map[string]any{"tag": "api", "services": []string{"HandlerService"}},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": apiPort, "protocol": "tunnel", "settings": map[string]any{"rewriteAddress": "127.0.0.1"}, "tag": "api"},
			map[string]any{"listen": "127.0.0.1", "port": userPort, "protocol": "shadowsocks", "settings": map[string]any{"network": "udp", "clients": []any{map[string]any{"email": "first@example.test", "method": "aes-128-gcm", "password": "first-password"}, map[string]any{"email": "second@example.test", "method": "aes-128-gcm", "password": "second-password"}}}, "tag": "server"},
		},
		"outbounds": []any{map[string]any{"protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "network": "udp", "ip": []string{"127.0.0.1/32"}, "port": strconv.Itoa(echoPort)}}}, "tag": "direct"}},
		"routing":   map[string]any{"rules": []any{map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"}}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "xray.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-c", path)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	waitForPort(t, apiPort)
	api := XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	a, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(userPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(userPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	first, second := ssUDPUser(t, "first@example.test", "first-password"), ssUDPUser(t, "second@example.test", "second-password")
	ssUDPExchange(t, a, first, echoPort, "before", true)
	ssUDPExchange(t, b, second, echoPort, "other", true)
	if err := api.RemoveUser("server", "first@example.test"); err != nil {
		t.Fatal(err)
	}
	ssUDPExchange(t, b, second, echoPort, "alive", true)
	// The upstream core keeps using the cached user in the existing UDP
	// association; the experimental patch should close that association.
	ssUDPExchange(t, a, first, echoPort, "removed", os.Getenv("XRAY_EXPECT_EXPERIMENTAL_DRAIN") != "1")
	fresh, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(userPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	ssUDPExchange(t, fresh, first, echoPort, "fresh", false)
}

func ssUDPUser(t *testing.T, email, password string) *protocol.MemoryUser {
	t.Helper()
	a, err := (&shadowsocks.Account{Password: password, CipherType: shadowsocks.CipherType_AES_128_GCM}).AsAccount()
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.MemoryUser{Email: email, Account: a}
}

func ssUDPExchange(t *testing.T, conn net.Conn, user *protocol.MemoryUser, port int, text string, expect bool) {
	t.Helper()
	request := &protocol.RequestHeader{User: user, Command: protocol.RequestCommandUDP, Address: xnet.ParseAddress("127.0.0.1"), Port: xnet.Port(port)}
	packet, err := shadowsocks.EncodeUDPPacket(request, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Write(packet.Bytes())
	packet.Release()
	if err != nil {
		t.Fatal(err)
	}
	deadline := 3 * time.Second
	if !expect {
		deadline = 800 * time.Millisecond
	}
	if err := conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		t.Fatal(err)
	}
	raw := make([]byte, 2048)
	n, err := conn.Read(raw)
	if !expect {
		if err == nil {
			t.Fatalf("removed user received UDP packet %x", raw[:n])
		}
		if nerr, ok := err.(net.Error); !ok || !nerr.Timeout() {
			t.Fatalf("unexpected UDP error: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	payload := buf.New()
	defer payload.Release()
	_, _ = payload.Write(raw[:n])
	validator := new(shadowsocks.Validator)
	if err := validator.Add(user); err != nil {
		t.Fatal(err)
	}
	_, decoded, err := shadowsocks.DecodeUDPPacket(validator, payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Bytes()) != text {
		t.Fatalf("unexpected UDP response %q", decoded.Bytes())
	}
}

func TestExperimentalShadowsocks2022UDPDisconnect(t *testing.T) {
	bin := os.Getenv("XRAY_E2E_BINARY")
	if bin == "" || os.Getenv("XRAY_EXPECT_EXPERIMENTAL_DRAIN") != "1" {
		t.Skip("set XRAY_E2E_BINARY to an Xray binary")
	}
	echo, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		b := make([]byte, 2048)
		for {
			n, addr, err := echo.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = echo.WriteTo(b[:n], addr)
		}
	}()
	apiPort, userPort := freePort(t), freePort(t)
	if apiPort == userPort {
		t.Skip("ephemeral port collision")
	}
	echoPort := echo.LocalAddr().(*net.UDPAddr).Port
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"}, "api": map[string]any{"tag": "api", "services": []string{"HandlerService"}},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": apiPort, "protocol": "tunnel", "settings": map[string]any{"rewriteAddress": "127.0.0.1"}, "tag": "api"},
			map[string]any{"listen": "127.0.0.1", "port": userPort, "protocol": "shadowsocks", "settings": map[string]any{"network": "udp", "method": "2022-blake3-aes-128-gcm", "password": "ZmVkY2JhOTg3NjU0MzIxMA==", "clients": []any{map[string]any{"email": "first@example.test", "password": "MDEyMzQ1Njc4OWFiY2RlZg=="}, map[string]any{"email": "second@example.test", "password": "YWJjZGVmZ2hpamtsbW5vcA=="}}}, "tag": "server"},
		},
		"outbounds": []any{map[string]any{"protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "network": "udp", "ip": []string{"127.0.0.1/32"}, "port": strconv.Itoa(echoPort)}}}, "tag": "direct"}},
		"routing":   map[string]any{"rules": []any{map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"}}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "xray.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-c", path)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	waitForPort(t, apiPort)
	api := XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	a := ss2022UDPClient(t, userPort, "ZmVkY2JhOTg3NjU0MzIxMA==:MDEyMzQ1Njc4OWFiY2RlZg==")
	defer a.Close()
	b := ss2022UDPClient(t, userPort, "ZmVkY2JhOTg3NjU0MzIxMA==:YWJjZGVmZ2hpamtsbW5vcA==")
	defer b.Close()
	ss2022UDPExchange(t, a, echo.LocalAddr(), "before", true)
	ss2022UDPExchange(t, b, echo.LocalAddr(), "other", true)
	if err := api.RemoveUser("server", "first@example.test"); err != nil {
		t.Fatal(err)
	}
	ss2022UDPExchange(t, b, echo.LocalAddr(), "alive", true)
	ss2022UDPExchange(t, a, echo.LocalAddr(), "removed", false)
	fresh := ss2022UDPClient(t, userPort, "ZmVkY2JhOTg3NjU0MzIxMA==:MDEyMzQ1Njc4OWFiY2RlZg==")
	defer fresh.Close()
	ss2022UDPExchange(t, fresh, echo.LocalAddr(), "fresh", false)
}

func ss2022UDPClient(t *testing.T, port int, password string) net.PacketConn {
	t.Helper()
	method, err := shadowaead_2022.NewWithPassword("2022-blake3-aes-128-gcm", password, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	return method.DialPacketConn(conn)
}

func ss2022UDPExchange(t *testing.T, conn net.PacketConn, target net.Addr, text string, expect bool) {
	t.Helper()
	if _, err := conn.WriteTo([]byte(text), target); err != nil {
		t.Fatal(err)
	}
	deadline := 3 * time.Second
	if !expect {
		deadline = 800 * time.Millisecond
	}
	if err := conn.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2048)
	n, _, err := conn.ReadFrom(b)
	if !expect {
		if err == nil {
			t.Fatalf("removed user received UDP packet: %q", b[:n])
		}
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Fatalf("unexpected UDP error: %v", err)
		}
		return
	}
	if err != nil || string(b[:n]) != text {
		t.Fatalf("UDP echo %q: %q %v", text, b[:n], err)
	}
}
