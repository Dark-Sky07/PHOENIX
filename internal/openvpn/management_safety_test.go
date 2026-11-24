package openvpn

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
)

const tabHeader = "HEADER\tCLIENT_LIST\tCommon Name\tReal Address\tVirtual Address\tVirtual IPv6 Address\tBytes Received\tBytes Sent\tConnected Since\tConnected Since (time_t)\tUsername\tClient ID\tPeer ID\tData Channel Cipher\n"
const tabClient = "CLIENT_LIST\talice@example.test\t203.0.113.1:2222\t10.8.0.2\t\t1234\t5678\tSun Sep 20 08:00:00 2026\t179\tUNDEF\t1\t1\tAES-256-GCM\n"

func TestManagementStatusFormatsAndUnsafeRows(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		count      int
		unsafe     bool
	}{
		{"status3", "TITLE\tOpenVPN 2.6\nTIME\tdate\t1\n" + tabHeader + tabClient + "HEADER\tROUTING_TABLE\tVirtual Address\tCommon Name\nROUTING_TABLE\t10.8.0.2\talice@example.test\nEND\n", 1, false},
		{"status3 empty", tabHeader + "END\n", 0, false},
		{"status2 empty", "HEADER,CLIENT_LIST,Common Name,Real Address,Virtual Address,Virtual IPv6 Address,Bytes Received,Bytes Sent\nEND\n", 0, false},
		{"status1", "OpenVPN CLIENT LIST\nUpdated,now\nCommon Name,Real Address,Bytes Received,Bytes Sent,Connected Since\nalice@example.test,203.0.113.1:2222,1234,5678,now\nROUTING TABLE\nVirtual Address,Common Name,Real Address,Last Ref\n10.8.0.2,alice@example.test,203.0.113.1:2222,now\nGLOBAL STATS\nEND\n", 1, false},
		{"truncated", tabHeader + tabClient, 0, true},
		{"duplicate header column", "HEADER\tCLIENT_LIST\tCommon Name\tBytes Received\tBytes Sent\tBytes Sent\nEND\n", 0, true},
		{"invalid counter", tabHeader + strings.Replace(tabClient, "1234", "bad", 1) + "END\n", 0, true},
		{"short row", tabHeader + "CLIENT_LIST\talice@example.test\nEND\n", 0, true},
		{"blank identity", tabHeader + strings.Replace(tabClient, "alice@example.test", "", 1) + "END\n", 0, true},
		{"duplicate identity", tabHeader + tabClient + strings.Replace(tabClient, "alice@example.test", "ALICE@example.test", 1) + "END\n", 0, true},
		{"missing header", tabClient + "END\n", 0, true},
		{"incomplete header", "HEADER\tCLIENT_LIST\tCommon Name\tBytes Received\nEND\n", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseClientListResponse(strings.NewReader(tc.text))
			if tc.unsafe {
				if !errors.Is(err, errUnsafeManagementResponse) || got != nil {
					t.Fatalf("accepted unsafe response: %v %v", got, err)
				}
				return
			}
			if err != nil || len(got) != tc.count {
				t.Fatalf("unexpected response: %v %v", got, err)
			}
			if tc.count == 1 && got["alice@example.test"] != (clientCounters{Rx: 1234, Tx: 5678}) {
				t.Fatalf("bad counters: %v", got)
			}
		})
	}
	if _, err := parseClientListResponse(strings.NewReader("OK\nEND\n")); err == nil {
		t.Fatal("unknown response accepted as empty")
	}
}

// Exercise the real management socket path, including fallback behavior and
// the baseline retained by CollectTraffic when a subsequent row is malformed.
func TestStatusThreePollRetainsBaselineOnInvalidResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	response := tabHeader + tabClient + "END\n"
	commands := []string{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				cmd, _ := bufio.NewReader(conn).ReadString('\n')
				mu.Lock()
				commands = append(commands, strings.TrimSpace(cmd))
				text := response
				mu.Unlock()
				_, _ = fmt.Fprint(conn, text)
			}()
		}
	}()
	defer func() { ln.Close(); <-done }()
	m := &Manager{procs: map[int]*managed{1: {tag: "ovpn", mgmtPort: ln.Addr().(*net.TCPAddr).Port}}}
	ins, clients := m.CollectTraffic()
	if len(ins) != 1 || len(clients) != 1 || clients[0].Up != 1234 || clients[0].Down != 5678 {
		t.Fatalf("first poll: %v %v", ins, clients)
	}
	mu.Lock()
	response = tabHeader + strings.Replace(tabClient, "1234", "bad", 1) + "END\n"
	mu.Unlock()
	ins, clients = m.CollectTraffic()
	if len(ins) != 0 || len(clients) != 0 || m.procs[1].lastRx["alice@example.test"] != 1234 {
		t.Fatalf("invalid poll lost baseline: %v %v %+v", ins, clients, m.procs[1].lastRx)
	}
	mu.Lock()
	response = tabHeader + strings.Replace(tabClient, "1234", "1300", 1) + "END\n"
	count := len(commands)
	mu.Unlock()
	if count != 2 {
		t.Fatalf("unsafe response retried with another command: %v", commands)
	}
	ins, clients = m.CollectTraffic()
	if len(ins) != 1 || len(clients) != 1 || clients[0].Up != 66 || clients[0].Down != 0 {
		t.Fatalf("retry did not retain baseline: %v %v", ins, clients)
	}
}
