package openvpn

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This test demonstrates an accounting blind spot, not a production fix.
// The disconnect script is a temporary experiment and is not installed by PHOENIX.
func TestRealOpenVPNShortSessionBetweenPolls(t *testing.T) {
	binary := os.Getenv("PHOENIX_TEST_OPENVPN_LIVE_BINARY")
	if binary == "" {
		t.Skip("PHOENIX_TEST_OPENVPN_LIVE_BINARY not configured")
	}
	serverNS, clientNS := setupOpenVPNLiveNamespaces(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	inst := Instance{Id: 121, Port: 31394, Listen: "192.0.2.1", Proto: "udp"}
	profile, err := BuildProfile(inst, "short@example.test", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeConfig(inst, 31395); err != nil {
		t.Fatal(err)
	}
	stripProductionHooks(t, inst)
	dir := t.TempDir()
	record := filepath.Join(dir, "disconnect")
	hook := filepath.Join(dir, "hook")
	script := "#!/bin/sh\nprintf '%s\\t%s\\t%s\\n' \"${common_name:-}\" \"${bytes_received:-}\" \"${bytes_sent:-}\" > " + strconv.Quote(record) + "\n"
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(confPathForID(inst.Id), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := fmt.Fprintf(f, "disable-dco\nscript-security 2\nclient-disconnect %s\n", hook)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("write server config: %v %v", writeErr, closeErr)
	}
	start := func(ns, path string) (*exec.Cmd, <-chan struct{}) {
		t.Helper()
		cmd := exec.Command("ip", "netns", "exec", ns, binary, "--config", path)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		return cmd, done
	}
	waitFor := func(path, text string) {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			data, _ := os.ReadFile(path)
			if strings.Contains(string(data), text) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		data, _ := os.ReadFile(path)
		server, _ := os.ReadFile(logPathForID(inst.Id))
		t.Fatalf("missing %q in %s: %s\nserver:\n%s", text, path, data, server)
	}
	start(serverNS, confPathForID(inst.Id))
	waitFor(logPathForID(inst.Id), "Initialization Sequence Completed")
	management := func() string {
		t.Helper()
		const code = `import socket,sys
s=socket.create_connection(('127.0.0.1',31395),timeout=4)
s.settimeout(4)
s.sendall(b'status 3\n')
b=b''
while True:
 p=s.recv(65536)
 if not p: break
 b+=p
 if any(x.rstrip(b'\r') == b'END' for x in b.split(b'\n')[:-1]): break
sys.stdout.buffer.write(b)
`
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "ip", "netns", "exec", serverNS, "python3", "-c", code).CombinedOutput()
		if err != nil {
			t.Fatalf("management status: %v %s", err, out)
		}
		return strings.ReplaceAll(string(out), "\r\n", "\n")
	}
	before := management()
	if clients, err := parseClientListResponse(strings.NewReader(before)); err != nil || len(clients) != 0 {
		t.Fatalf("initial status not empty: %v %v %s", clients, err, before)
	}
	clientLog := filepath.Join(dir, "client.log")
	clientConf := filepath.Join(dir, "client.ovpn")
	if err := os.WriteFile(clientConf, []byte(profile+"\ndisable-dco\nexplicit-exit-notify 1\nlog "+quoteConfigPath(clientLog)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, clientDone := start(clientNS, clientConf)
	waitFor(clientLog, "Initialization Sequence Completed")
	probe, err := exec.Command("ip", "netns", "exec", clientNS, "ping", "-n", "-c", "2", "-W", "3", "10.121.0.1").CombinedOutput()
	if err != nil {
		t.Fatalf("short session did not carry traffic: %v %s", err, probe)
	}
	if err := client.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-clientDone:
	case <-time.After(12 * time.Second):
		t.Fatal("client did not exit")
	}
	waitFor(record, "short@example.test\t")
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSpace(string(data)), "\t")
	if len(fields) != 3 || fields[0] != "short@example.test" {
		t.Fatalf("invalid hook record: %q", data)
	}
	rx, errRx := strconv.ParseUint(fields[1], 10, 64)
	tx, errTx := strconv.ParseUint(fields[2], 10, 64)
	if errRx != nil || errTx != nil || rx == 0 || tx == 0 {
		t.Fatalf("hook lacks bidirectional counters: %q", data)
	}
	var after string
	deadline := time.Now().Add(10 * time.Second)
	for {
		after = management()
		clients, err := parseClientListResponse(strings.NewReader(after))
		if err != nil {
			t.Fatalf("invalid final status: %v %s", err, after)
		}
		if len(clients) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session not absent: %s", after)
		}
		time.Sleep(100 * time.Millisecond)
	}
	// No production poll occurs while the session is active. Replaying the
	// two real snapshots through the production manager exposes the gap.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, response := range []string{before, after} {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = fmt.Fprint(conn, response)
			_ = conn.Close()
		}
	}()
	defer func() { _ = ln.Close(); <-done }()
	manager := &Manager{procs: map[int]*managed{inst.Id: {tag: "short", mgmtPort: ln.Addr().(*net.TCPAddr).Port}}}
	for i := 0; i < 2; i++ {
		in, clients, err := manager.CollectTrafficWithErrors()
		if err != nil || len(in) != 0 || len(clients) != 0 || manager.Status().Error != "" {
			t.Fatalf("unexpected production observation on poll %d: %v %v %v %+v", i, in, clients, err, manager.Status())
		}
	}
}
