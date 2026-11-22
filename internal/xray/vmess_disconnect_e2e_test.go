package xray

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	M "github.com/sagernet/sing/common/metadata"
)

// This covers VMess over TCP with an Xray SOCKS client, not other transports,
// mux modes or final counter durability.
func TestExperimentalVMessDisconnect(t *testing.T) {
	bin := os.Getenv("XRAY_E2E_BINARY")
	if bin == "" || os.Getenv("XRAY_EXPECT_EXPERIMENTAL_DRAIN") != "1" {
		t.Skip("experimental Xray required")
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4096)
				for {
					n, err := c.Read(b)
					if n > 0 {
						if _, e := c.Write(b[:n]); e != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	apiPort, serverPort, clientA, clientB := freePort(t), freePort(t), freePort(t), freePort(t)
	ports := map[int]bool{}
	for _, p := range []int{apiPort, serverPort, clientA, clientB} {
		if ports[p] {
			t.Skip("ephemeral port collision")
		}
		ports[p] = true
	}
	firstID, secondID := "a17e367c-2074-4d3e-aaeb-fbef5dfde7e7", "9b7e367c-2074-4d3e-aaeb-fbef5dfde7e7"
	echoPort := echo.Addr().(*net.TCPAddr).Port
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"api": map[string]any{"tag": "api", "services": []string{"HandlerService"}},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": apiPort, "protocol": "tunnel", "settings": map[string]any{"rewriteAddress": "127.0.0.1"}, "tag": "api"},
			map[string]any{"listen": "127.0.0.1", "port": serverPort, "protocol": "vmess", "settings": map[string]any{"clients": []any{map[string]any{"id": firstID, "email": "first@example.test"}, map[string]any{"id": secondID, "email": "second@example.test"}}}, "tag": "server"},
			map[string]any{"listen": "127.0.0.1", "port": clientA, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}, "tag": "a"},
			map[string]any{"listen": "127.0.0.1", "port": clientB, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}, "tag": "b"},
		},
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "network": "tcp", "ip": []string{"127.0.0.1/32"}, "port": strconv.Itoa(echoPort)}}}, "tag": "direct"},
			vmessTestOutbound("out-a", firstID, serverPort), vmessTestOutbound("out-b", secondID, serverPort),
		},
		"routing": map[string]any{"rules": []any{
			map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"},
			map[string]any{"type": "field", "inboundTag": []string{"a"}, "outboundTag": "out-a"},
			map[string]any{"type": "field", "inboundTag": []string{"b"}, "outboundTag": "out-b"},
		}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "xray.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-c", config)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	for _, port := range []int{apiPort, serverPort, clientA, clientB} {
		waitForPort(t, port)
	}
	api := XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	a := vmessTestSOCKS(t, clientA, echoPort)
	defer a.Close()
	b := vmessTestSOCKS(t, clientB, echoPort)
	defer b.Close()
	vmessTestEcho(t, a, "before")
	vmessTestEcho(t, b, "other")
	if err := api.RemoveUser("server", "first@example.test"); err != nil {
		t.Fatal(err)
	}
	vmessTestEcho(t, b, "alive")
	_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [1]byte
	if _, err := a.Read(buf[:]); err == nil {
		t.Fatal("removed VMess session still active")
	} else if n, ok := err.(net.Error); ok && n.Timeout() {
		t.Fatalf("removed VMess session did not close: %v", err)
	}
	assertNoImmediateForwardingAfterRemoval(t, clientA, echoPort)
}

func vmessTestOutbound(tag, id string, port int) map[string]any {
	return map[string]any{"tag": tag, "protocol": "vmess", "settings": map[string]any{"vnext": []any{map[string]any{"address": "127.0.0.1", "port": port, "users": []any{map[string]any{"id": id, "security": "auto"}}}}}}
}

func vmessTestSOCKS(t *testing.T, port, target int) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(c, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("SOCKS method: %v %v", method, err)
	}
	if _, err := c.Write([]byte{5, 1, 0, 1, 127, 0, 0, 1, byte(target >> 8), byte(target)}); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil || reply[1] != 0 {
		t.Fatalf("SOCKS connect: %v %v", reply, err)
	}
	return c
}

func vmessTestEcho(t *testing.T, c net.Conn, text string) {
	t.Helper()
	if _, err := c.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(text))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != text {
		t.Fatalf("echo %q: %q %v", text, got, err)
	}
}

func TestExperimentalShadowsocksTCPDisconnect(t *testing.T) {
	bin := os.Getenv("XRAY_E2E_BINARY")
	if bin == "" || os.Getenv("XRAY_EXPECT_EXPERIMENTAL_DRAIN") != "1" {
		t.Skip("experimental Xray required")
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4096)
				for {
					n, err := c.Read(b)
					if n > 0 {
						if _, e := c.Write(b[:n]); e != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	apiPort, serverPort, clientA, clientB := freePort(t), freePort(t), freePort(t), freePort(t)
	ports := map[int]bool{}
	for _, p := range []int{apiPort, serverPort, clientA, clientB} {
		if ports[p] {
			t.Skip("ephemeral port collision")
		}
		ports[p] = true
	}
	firstID, secondID := "first-password", "second-password"
	echoPort := echo.Addr().(*net.TCPAddr).Port
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"api": map[string]any{"tag": "api", "services": []string{"HandlerService"}},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": apiPort, "protocol": "tunnel", "settings": map[string]any{"rewriteAddress": "127.0.0.1"}, "tag": "api"},
			map[string]any{"listen": "127.0.0.1", "port": serverPort, "protocol": "shadowsocks", "settings": map[string]any{"network": "tcp", "clients": []any{map[string]any{"password": firstID, "method": "aes-128-gcm", "email": "first@example.test"}, map[string]any{"password": secondID, "method": "aes-128-gcm", "email": "second@example.test"}}}, "tag": "server"},
			map[string]any{"listen": "127.0.0.1", "port": clientA, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}, "tag": "a"},
			map[string]any{"listen": "127.0.0.1", "port": clientB, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}, "tag": "b"},
		},
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "network": "tcp", "ip": []string{"127.0.0.1/32"}, "port": strconv.Itoa(echoPort)}}}, "tag": "direct"},
			shadowsocksTestOutbound("out-a", firstID, serverPort), shadowsocksTestOutbound("out-b", secondID, serverPort),
		},
		"routing": map[string]any{"rules": []any{
			map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"},
			map[string]any{"type": "field", "inboundTag": []string{"a"}, "outboundTag": "out-a"},
			map[string]any{"type": "field", "inboundTag": []string{"b"}, "outboundTag": "out-b"},
		}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "xray.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-c", config)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	for _, port := range []int{apiPort, serverPort, clientA, clientB} {
		waitForPort(t, port)
	}
	api := XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	a := vmessTestSOCKS(t, clientA, echoPort)
	defer a.Close()
	b := vmessTestSOCKS(t, clientB, echoPort)
	defer b.Close()
	vmessTestEcho(t, a, "before")
	vmessTestEcho(t, b, "other")
	if err := api.RemoveUser("server", "first@example.test"); err != nil {
		t.Fatal(err)
	}
	vmessTestEcho(t, b, "alive")
	_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [1]byte
	if _, err := a.Read(buf[:]); err == nil {
		t.Fatal("removed Shadowsocks TCP session still active")
	} else if n, ok := err.(net.Error); ok && n.Timeout() {
		t.Fatalf("removed Shadowsocks TCP session did not close: %v", err)
	}
	assertNoImmediateForwardingAfterRemoval(t, clientA, echoPort)
}

func shadowsocksTestOutbound(tag, password string, port int) map[string]any {
	return map[string]any{"tag": tag, "protocol": "shadowsocks", "settings": map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "port": port, "method": "aes-128-gcm", "password": password}}}}
}

// SOCKS acknowledges before upstream authentication. Xray can deliberately
// delay rejection for anti-probing; a timeout shows only that no response was
// forwarded during this bounded observation, not that the connection closed.
func assertNoImmediateForwardingAfterRemoval(t *testing.T, socksPort, target int) {
	t.Helper()
	c := vmessTestSOCKS(t, socksPort, target)
	defer c.Close()
	if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("retry")); err != nil {
		return
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err == nil {
		t.Fatal("removed credential forwarded a new connection")
	} else if n, ok := err.(net.Error); ok && n.Timeout() {
		t.Log("no forwarding observed before the deadline; connection termination is not established")
	}
}

func TestExperimentalShadowsocks2022TCPDisconnect(t *testing.T) {
	bin := os.Getenv("XRAY_E2E_BINARY")
	if bin == "" || os.Getenv("XRAY_EXPECT_EXPERIMENTAL_DRAIN") != "1" {
		t.Skip("experimental Xray required")
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b := make([]byte, 4096)
				for {
					n, err := c.Read(b)
					if n > 0 {
						if _, e := c.Write(b[:n]); e != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	apiPort, serverPort, clientA, clientB := freePort(t), freePort(t), freePort(t), freePort(t)
	ports := map[int]bool{}
	for _, p := range []int{apiPort, serverPort, clientA, clientB} {
		if ports[p] {
			t.Skip("ephemeral port collision")
		}
		ports[p] = true
	}
	serverKey := "ZmVkY2JhOTg3NjU0MzIxMA=="
	firstID, secondID := "MDEyMzQ1Njc4OWFiY2RlZg==", "YWJjZGVmZ2hpamtsbW5vcA=="
	echoPort := echo.Addr().(*net.TCPAddr).Port
	cfg := map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"api": map[string]any{"tag": "api", "services": []string{"HandlerService"}},
		"inbounds": []any{
			map[string]any{"listen": "127.0.0.1", "port": apiPort, "protocol": "tunnel", "settings": map[string]any{"rewriteAddress": "127.0.0.1"}, "tag": "api"},
			map[string]any{"listen": "127.0.0.1", "port": serverPort, "protocol": "shadowsocks", "settings": map[string]any{"network": "tcp", "method": "2022-blake3-aes-128-gcm", "password": serverKey, "clients": []any{map[string]any{"password": firstID, "email": "first@example.test"}, map[string]any{"password": secondID, "email": "second@example.test"}}}, "tag": "server"},
			map[string]any{"listen": "127.0.0.1", "port": clientA, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}, "tag": "a"},
			map[string]any{"listen": "127.0.0.1", "port": clientB, "protocol": "socks", "settings": map[string]any{"auth": "noauth"}, "tag": "b"},
		},
		"outbounds": []any{
			map[string]any{"protocol": "freedom", "settings": map[string]any{"finalRules": []any{map[string]any{"action": "allow", "network": "tcp", "ip": []string{"127.0.0.1/32"}, "port": strconv.Itoa(echoPort)}}}, "tag": "direct"},
			shadowsocks2022TestOutbound("out-a", serverKey+":"+firstID, serverPort), shadowsocks2022TestOutbound("out-b", serverKey+":"+secondID, serverPort),
		},
		"routing": map[string]any{"rules": []any{
			map[string]any{"type": "field", "inboundTag": []string{"api"}, "outboundTag": "api"},
			map[string]any{"type": "field", "inboundTag": []string{"a"}, "outboundTag": "out-a"},
			map[string]any{"type": "field", "inboundTag": []string{"b"}, "outboundTag": "out-b"},
		}},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "xray.json")
	if err := os.WriteFile(config, data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-c", config)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	for _, port := range []int{apiPort, serverPort, clientA, clientB} {
		waitForPort(t, port)
	}
	api := XrayAPI{}
	if err := api.Init(apiPort); err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	a := ss2022DirectTCP(t, serverPort, serverKey+":"+firstID, echoPort)
	defer a.Close()
	b := ss2022DirectTCP(t, serverPort, serverKey+":"+secondID, echoPort)
	defer b.Close()
	vmessTestEcho(t, a, "before")
	vmessTestEcho(t, b, "other")
	if err := api.RemoveUser("server", "first@example.test"); err != nil {
		t.Fatal(err)
	}
	vmessTestEcho(t, b, "alive")
	// Removing the first user changes the list positions. A fresh second-user
	// authentication must still resolve to the original second user.
	freshB := ss2022DirectTCP(t, serverPort, serverKey+":"+secondID, echoPort)
	defer freshB.Close()
	vmessTestEcho(t, freshB, "fresh-second")
	_ = a.SetReadDeadline(time.Now().Add(3 * time.Second))
	var buf [1]byte
	if _, err := a.Read(buf[:]); err == nil {
		t.Fatal("removed Shadowsocks 2022 TCP session still active")
	} else if n, ok := err.(net.Error); ok && n.Timeout() {
		t.Fatalf("removed Shadowsocks 2022 TCP session did not close: %v", err)
	}
	// The direct socket observes the actual encrypted transport, without a
	// local SOCKS proxy's independent half-close and timeout behavior.
}

func shadowsocks2022TestOutbound(tag, password string, port int) map[string]any {
	return map[string]any{"tag": tag, "protocol": "shadowsocks", "settings": map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "port": port, "method": "2022-blake3-aes-128-gcm", "password": password}}}}
}

func ss2022DirectTCP(t *testing.T, serverPort int, password string, targetPort int) net.Conn {
	t.Helper()
	method, err := shadowaead_2022.NewWithPassword("2022-blake3-aes-128-gcm", password, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(serverPort)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := method.DialConn(raw, M.ParseSocksaddr(net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort))))
	if err != nil {
		raw.Close()
		t.Fatal(err)
	}
	return conn
}
