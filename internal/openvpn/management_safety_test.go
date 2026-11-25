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
		{"error after client header", tabHeader + "ERROR: interrupted\n", 0, true},
		{"error after client row", tabHeader + tabClient + "ERROR: interrupted\n", 0, true},
		{"error after legacy version", "<CLIENT_LIST VERSION 1\nERROR: interrupted\n", 0, true},
		{"duplicate header column", "HEADER\tCLIENT_LIST\tCommon Name\tBytes Received\tBytes Sent\tBytes Sent\nEND\n", 0, true},
		{"duplicate client header", tabHeader + tabClient + tabHeader + "END\n", 0, true},
		{"conflicting client header", tabHeader + "HEADER\tCLIENT_LIST\tCommon Name\tBytes Sent\tBytes Received\nEND\n", 0, true},
		{"mixed structured and space rows", tabHeader + "CLIENT_LIST alice@example.test 203.0.113.1 10.8.0.2 1234 5678 179\nEND\n", 0, true},
		{"duplicate legacy header", "<CLIENT_LIST HEADER ID Common Name Real Address Virtual Address Bytes Received Bytes Sent Connected Since\n<CLIENT_LIST HEADER ID Common Name Real Address Virtual Address Bytes Received Bytes Sent Connected Since\n<END\n", 0, true},
		{"mixed legacy and structured headers", "<CLIENT_LIST HEADER ID Common Name Real Address Virtual Address Bytes Received Bytes Sent Connected Since\n" + tabHeader + "END\n", 0, true},
		{"invalid counter", tabHeader + strings.Replace(tabClient, "1234", "bad", 1) + "END\n", 0, true},
		{"counter outside signed range", tabHeader + strings.Replace(tabClient, "1234", "9223372036854775808", 1) + "END\n", 0, true},
		{"legacy bad counter before numeric fields", "CLIENT_LIST alice@example.test 203.0.113.1 10.8.0.2 bad 1234 5678 179\nEND\n", 0, true},
		{"legacy bad sent counter before numeric fields", "CLIENT_LIST alice@example.test 203.0.113.1 10.8.0.2 1234 bad 5678 179\nEND\n", 0, true},
		{"short row", tabHeader + "CLIENT_LIST\talice@example.test\nEND\n", 0, true},
		{"bare row marker", tabHeader + "CLIENT_LIST\nEND\n", 0, true},
		{"empty tab row", tabHeader + "CLIENT_LIST\t\nEND\n", 0, true},
		{"bare legacy marker", "<CLIENT_LIST HEADER ID Common Name Real Address Virtual Address Bytes Received Bytes Sent Connected Since\n<CLIENT_LIST\n<END\n", 0, true},
		{"truncated legacy row", "OpenVPN CLIENT LIST\nCommon Name,Real Address,Bytes Received,Bytes Sent,Connected Since\nalice@example.test\nEND\n", 0, true},
		{"blank identity", tabHeader + strings.Replace(tabClient, "alice@example.test", "", 1) + "END\n", 0, true},
		{"duplicate identity", tabHeader + tabClient + strings.Replace(tabClient, "alice@example.test", "ALICE@example.test", 1) + "END\n", 0, true},
		{"missing header", tabClient + "END\n", 0, true},
		{"legacy status without header", "OpenVPN CLIENT LIST\nUpdated,now\nEND\n", 0, true},
		{"legacy version without header", "<CLIENT_LIST VERSION 1\n<END\n", 0, true},
		{"legacy incomplete header", "<CLIENT_LIST HEADER ID Common Name\n<END\n", 0, true},
		{"legacy empty with header", "<CLIENT_LIST VERSION 1\n<CLIENT_LIST HEADER ID Common Name Real Address Virtual Address Bytes Received Bytes Sent Connected Since\n<END\n", 0, false},
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
	for _, bad := range []string{
		"OpenVPN CLIENT LIST\nUpdated,now\nEND\n",
		tabHeader + strings.Replace(tabClient, "1234", "9223372036854775808", 1) + "END\n",
		"CLIENT_LIST alice@example.test 203.0.113.1 10.8.0.2 bad 1234 5678 179\nEND\n",
		tabHeader + "CLIENT_LIST\t\nEND\n",
		tabHeader + tabClient + "ERROR: interrupted\n",
		tabHeader + tabClient + "HEADER\tCLIENT_LIST\tCommon Name\tBytes Sent\tBytes Received\nEND\n",
		tabHeader + "CLIENT_LIST alice@example.test 203.0.113.1 10.8.0.2 1234 5678 179\nEND\n",
		"OpenVPN CLIENT LIST\nCommon Name,Real Address,Bytes Received,Bytes Sent,Connected Since\nalice@example.test\nEND\n",
	} {
		mu.Lock()
		response = bad
		mu.Unlock()
		ins, clients = m.CollectTraffic()
		if len(ins) != 0 || len(clients) != 0 || m.procs[1].lastRx["alice@example.test"] != 1234 {
			t.Fatalf("invalid poll lost baseline: %v %v %+v", ins, clients, m.procs[1].lastRx)
		}
	}
	mu.Lock()
	response = tabHeader + strings.Replace(tabClient, "1234", "1300", 1) + "END\n"
	count := len(commands)
	mu.Unlock()
	if count != 9 {
		t.Fatalf("unsafe response retried with another command: %v", commands)
	}
	ins, clients = m.CollectTraffic()
	if len(ins) != 1 || len(clients) != 1 || clients[0].Up != 66 || clients[0].Down != 0 {
		t.Fatalf("retry did not retain baseline: %v %v", ins, clients)
	}
}

func TestPartialManagementPollReturnsObservedDeltasAndError(t *testing.T) {
	good := &fakeMgmtServer{clients: map[string]clientCounters{"good@example.test": {Rx: 44, Tx: 19}}}
	good.start(t)
	defer good.close()
	bad := &fakeMgmtServer{}
	bad.start(t)
	port := bad.port
	bad.close()
	m := &Manager{procs: map[int]*managed{
		1: {tag: "good", mgmtPort: good.port},
		2: {tag: "bad", mgmtPort: port, lastRx: map[string]int64{"bad@example.test": 72}, lastTx: map[string]int64{"bad@example.test": 5}, haveLast: true},
	}}
	ins, clients, err := m.CollectTrafficWithErrors()
	if err == nil || len(ins) != 1 || len(clients) != 1 || clients[0].Email != "good@example.test" || clients[0].Up != 44 || clients[0].Down != 19 {
		t.Fatalf("partial result lost: %v %v %v", ins, clients, err)
	}
	if m.procs[2].lastRx["bad@example.test"] != 72 {
		t.Fatal("failed daemon baseline lost")
	}
}

// A replacement while the management query is blocked discards that daemon's
// sample, but must not make the overall poll look complete to the scheduler.
func TestManagementPollReportsDaemonReplacement(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	request := make(chan struct{})
	release := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = bufio.NewReader(conn).ReadString('\n')
		close(request)
		<-release
		_, _ = fmt.Fprint(conn, tabHeader+tabClient+"END\n")
	}()
	old := &managed{tag: "old", mgmtPort: ln.Addr().(*net.TCPAddr).Port, haveLast: true, lastRx: map[string]int64{"alice@example.test": 100}, lastTx: map[string]int64{"alice@example.test": 200}}
	replacement := &managed{tag: "new", lastRx: map[string]int64{"other@example.test": 9}}
	m := &Manager{procs: map[int]*managed{1: old}}
	type result struct {
		inbound []InboundTraffic
		clients []ClientTrafficDelta
		err     error
	}
	resultCh := make(chan result, 1)
	go func() { in, clients, err := m.CollectTrafficWithErrors(); resultCh <- result{in, clients, err} }()
	<-request
	m.mu.Lock()
	m.procs[1] = replacement
	m.mu.Unlock()
	close(release)
	got := <-resultCh
	<-serverDone
	if got.err == nil || !strings.Contains(got.err.Error(), "daemon changed during management traffic poll") || len(got.inbound) != 0 || len(got.clients) != 0 {
		t.Fatalf("discarded sample reported as complete: %+v", got)
	}
	if status := m.Status().Error; !strings.Contains(status, "final usage is unverified") {
		t.Fatalf("replacement hid unverified interval: %q", status)
	}
	if old.lastRx["alice@example.test"] != 100 || replacement.lastRx["other@example.test"] != 9 || len(replacement.lastRx) != 1 {
		t.Fatalf("sample mutated old or replacement baseline: old=%v new=%v", old.lastRx, replacement.lastRx)
	}
}

func TestManagementAggregateOverflowPreservesWholeDaemonBaseline(t *testing.T) {
	for _, direction := range []string{"received", "sent"} {
		t.Run(direction, func(t *testing.T) {
			bad := &fakeMgmtServer{clients: map[string]clientCounters{
				"alice@example.test": {Rx: 10, Tx: 10},
				"bob@example.test":   {Rx: 0, Tx: 0},
			}}
			bad.start(t)
			defer bad.close()
			good := &fakeMgmtServer{clients: map[string]clientCounters{"good@example.test": {Rx: 3, Tx: 4}}}
			good.start(t)
			defer good.close()
			man := &managed{tag: "overflow", mgmtPort: bad.port, haveLast: true,
				lastRx: map[string]int64{"alice@example.test": 10, "bob@example.test": 0},
				lastTx: map[string]int64{"alice@example.test": 10, "bob@example.test": 0}}
			m := &Manager{procs: map[int]*managed{1: man, 2: {tag: "good", mgmtPort: good.port}}}
			if direction == "received" {
				bad.setClient("alice@example.test", clientCounters{Rx: maxManagementCounter, Tx: 10})
				bad.setClient("bob@example.test", clientCounters{Rx: 11, Tx: 0})
			} else {
				bad.setClient("alice@example.test", clientCounters{Rx: 10, Tx: maxManagementCounter})
				bad.setClient("bob@example.test", clientCounters{Rx: 0, Tx: 11})
			}
			ins, clients, err := m.CollectTrafficWithErrors()
			if err == nil || !strings.Contains(err.Error(), "delta sum exceeds supported range") || len(ins) != 1 || ins[0].InboundId != 2 || len(clients) != 1 || clients[0].Email != "good@example.test" {
				t.Fatalf("bad partial result: inbound=%v clients=%v err=%v", ins, clients, err)
			}
			if len(man.lastRx) != 2 || len(man.lastTx) != 2 || man.lastRx["alice@example.test"] != 10 || man.lastTx["alice@example.test"] != 10 || man.lastRx["bob@example.test"] != 0 || man.lastTx["bob@example.test"] != 0 {
				t.Fatalf("overflow advanced a baseline: rx=%v tx=%v", man.lastRx, man.lastTx)
			}
			bad.setClient("alice@example.test", clientCounters{Rx: 30, Tx: 40})
			bad.setClient("bob@example.test", clientCounters{Rx: 15, Tx: 25})
			ins, clients, err = m.CollectTrafficWithErrors()
			if err != nil || len(ins) != 1 || ins[0].InboundId != 1 || ins[0].Up != 35 || ins[0].Down != 55 || len(clients) != 2 {
				t.Fatalf("retry lost or duplicated deltas: inbound=%v clients=%v err=%v", ins, clients, err)
			}
		})
	}
}

func TestManagementAggregateAtLimitIsAcceptedExactly(t *testing.T) {
	server := &fakeMgmtServer{clients: map[string]clientCounters{
		"alice@example.test": {Rx: maxManagementCounter - 1, Tx: 1},
		"bob@example.test":   {Rx: 1, Tx: maxManagementCounter - 1},
	}}
	server.start(t)
	defer server.close()
	m := &Manager{procs: map[int]*managed{1: {tag: "ovpn", mgmtPort: server.port}}}
	ins, clients, err := m.CollectTrafficWithErrors()
	if err != nil || len(ins) != 1 || ins[0].Up != maxManagementCounter || ins[0].Down != maxManagementCounter || len(clients) != 2 {
		t.Fatalf("valid maximum rejected or altered: inbound=%v clients=%v err=%v", ins, clients, err)
	}
}

func TestManagementUnsupportedCommandFallsBackBeforeSnapshot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	commands := make(chan string, 2)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			cmd, _ := bufio.NewReader(conn).ReadString('\n')
			commands <- strings.TrimSpace(cmd)
			if i == 0 {
				fmt.Fprint(conn, ">INFO:management\nERROR: unsupported\n")
			} else {
				fmt.Fprint(conn, tabHeader+tabClient+"END\n")
			}
			conn.Close()
		}
	}()
	got, err := clientList(ln.Addr().(*net.TCPAddr).Port)
	if err != nil || got["alice@example.test"] != (clientCounters{Rx: 1234, Tx: 5678}) {
		t.Fatalf("legitimate fallback failed: %v %v", got, err)
	}
	<-serverDone
	first, second := <-commands, <-commands
	if first != "status 3" || second != "status 2" {
		t.Fatalf("unexpected fallback commands %q %q", first, second)
	}
}

func TestPollFailureStatusSurvivesUnrelatedSuccessAndResume(t *testing.T) {
	bad := &fakeMgmtServer{}
	bad.start(t)
	port := bad.port
	bad.close()
	good := &fakeMgmtServer{clients: map[string]clientCounters{"private@example.test": {Rx: 1}}}
	good.start(t)
	defer good.close()
	m := &Manager{procs: map[int]*managed{
		1: {tag: "bad", mgmtPort: port},
		2: {tag: "good", mgmtPort: good.port},
	}, lastError: "previous daemon error"}
	if _, _, err := m.CollectTrafficWithErrors(); err == nil {
		t.Fatal("missing poll failure")
	}
	status := m.Status().Error
	if !strings.Contains(status, "previous daemon error") || !strings.Contains(status, "traffic poll incomplete for 1 inbound(s)") || strings.Contains(status, "private@example.test") {
		t.Fatalf("unexpected dashboard status: %q", status)
	}
	m.Resume()
	if status = m.Status().Error; !strings.Contains(status, "traffic poll incomplete") {
		t.Fatalf("resume hid poll warning: %q", status)
	}
	// Removal or a successful poll of another daemon is not recovery evidence.
	m.mu.Lock()
	delete(m.procs, 1)
	m.mu.Unlock()
	if _, _, err := m.CollectTrafficWithErrors(); err != nil {
		t.Fatal(err)
	}
	if status = m.Status().Error; !strings.Contains(status, "traffic poll incomplete") {
		t.Fatalf("unrelated poll hid warning: %q", status)
	}
	m.mu.Lock()
	m.procs[1] = &managed{tag: "replacement", mgmtPort: good.port}
	m.mu.Unlock()
	if _, _, err := m.CollectTrafficWithErrors(); err != nil {
		t.Fatal(err)
	}
	if status = m.Status().Error; status != "" {
		t.Fatalf("successful same-inbound poll not reflected: %q", status)
	}
}

func TestUnverifiedOpenVPNSessionUsageRemainsVisible(t *testing.T) {
	for _, event := range []string{"disappeared", "received decreased", "sent decreased"} {
		t.Run(event, func(t *testing.T) {
			server := &fakeMgmtServer{clients: map[string]clientCounters{"private@example.test": {Rx: 100, Tx: 200}}}
			server.start(t)
			defer server.close()
			m := &Manager{procs: map[int]*managed{1: {tag: "vpn", mgmtPort: server.port}}}
			if _, _, err := m.CollectTrafficWithErrors(); err != nil {
				t.Fatal(err)
			}
			server.setClient("private@example.test", clientCounters{Rx: 110, Tx: 210})
			if _, _, err := m.CollectTrafficWithErrors(); err != nil || m.Status().Error != "" {
				t.Fatalf("normal increase caused warning: %q %v", m.Status().Error, err)
			}
			switch event {
			case "disappeared":
				server.clearClients()
			case "received decreased":
				server.setClient("private@example.test", clientCounters{Rx: 2, Tx: 220})
			case "sent decreased":
				server.setClient("private@example.test", clientCounters{Rx: 120, Tx: 2})
			}
			if _, _, err := m.CollectTrafficWithErrors(); err != nil {
				t.Fatal(err)
			}
			status := m.Status().Error
			if !strings.Contains(status, "final usage is unverified") || strings.Contains(status, "private@example.test") {
				t.Fatalf("missing or unsafe warning: %q", status)
			}
			// A later successful status or a resume does not prove the missing
			// interval was accounted for and must not clear this warning.
			m.Resume()
			server.setClient("private@example.test", clientCounters{Rx: 300, Tx: 300})
			if _, _, err := m.CollectTrafficWithErrors(); err != nil {
				t.Fatal(err)
			}
			if status = m.Status().Error; !strings.Contains(status, "final usage is unverified") {
				t.Fatalf("later success hid unverified usage: %q", status)
			}
		})
	}
}
