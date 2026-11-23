package l2tp

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func setupTrafficFilesystem(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	l2tpRootOverride = filepath.Join(root, "l2tp")
	l2tpSysClassNetRoot = filepath.Join(root, "sys", "class", "net")
	t.Cleanup(func() {
		l2tpRootOverride = ""
		l2tpSysClassNetRoot = "/sys/class/net"
	})
	return l2tpRootOverride, l2tpSysClassNetRoot
}

func writeSession(t *testing.T, id int, iface, email, rx, tx string, interfaceExists bool) {
	t.Helper()
	sessions := sessionDirForID(id)
	if err := os.MkdirAll(sessions, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, iface), []byte(email+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if !interfaceExists {
		return
	}
	stats := filepath.Join(l2tpSysClassNetRoot, iface, "statistics")
	if err := os.MkdirAll(stats, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stats, "rx_bytes"), []byte(rx+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stats, "tx_bytes"), []byte(tx+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestReadSessionsSupportsConcurrentSessionsForOneEmail(t *testing.T) {
	setupTrafficFilesystem(t)
	writeSession(t, 7, "ppp0", "alice@example.com", "100", "200", true)
	writeSession(t, 7, "ppp1", "alice@example.com", "300", "400", true)

	got, ok := readSessions(7)
	if !ok {
		t.Fatal("readSessions reported a transient failure")
	}
	if len(got) != 2 {
		t.Fatalf("read %d sessions, want two independent interface/email keys: %#v", len(got), got)
	}
	if got["ppp0\x00alice@example.com"].rx != 100 || got["ppp1\x00alice@example.com"].tx != 400 {
		t.Fatalf("session counters were not read independently: %#v", got)
	}
}

func TestReadSessionsDropsStaleMarkerWithoutInterface(t *testing.T) {
	setupTrafficFilesystem(t)
	writeSession(t, 8, "ppp-stale", "offline@example.com", "10", "20", false)

	got, ok := readSessions(8)
	if !ok {
		t.Fatal("a stale marker should be a healthy empty-session read")
	}
	if len(got) != 0 {
		t.Fatalf("stale marker produced an online session: %#v", got)
	}
}

func TestReadSessionsFailsClosedOnInvalidCounter(t *testing.T) {
	setupTrafficFilesystem(t)
	writeSession(t, 9, "ppp0", "alice@example.com", "not-a-counter", "20", true)

	got, ok := readSessions(9)
	if ok || got != nil {
		t.Fatalf("invalid interface counter was accepted: got=%#v ok=%v", got, ok)
	}
}

func TestReadCounterParsesUnsignedValuesWithoutNegativeWrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counter")
	if err := os.WriteFile(path, []byte("18446744073709551615\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	value, ok := readCounter(path)
	if !ok || value != maxInterfaceCounter {
		t.Fatalf("unsigned counter = (%d, %v), want saturated MaxInt64", value, ok)
	}

	if err := os.WriteFile(path, []byte("-1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if value, ok := readCounter(path); ok || value != 0 {
		t.Fatalf("negative counter was accepted: (%d, %v)", value, ok)
	}
}

func TestCollectTrafficCheckedFailurePreservesBaselines(t *testing.T) {
	setupTrafficFilesystem(t)
	writeSession(t, 12, "ppp0", "alice@example.com", "bad", "200", true)
	cmd := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	p := &Process{ipsecCmd: cmd, xl2tpdCmd: cmd}
	baseline := map[string]interfaceCounter{"ppp0\x00alice@example.com": {rx: 50, tx: 100}}
	m := &Manager{id: 12, proc: &managed{proc: p, tag: "tag", last: baseline}}
	if _, _, err := m.CollectTrafficChecked(); err == nil {
		t.Fatal("counter failure was not reported")
	}
	if got := baseline["ppp0\x00alice@example.com"]; got.rx != 50 || got.tx != 100 {
		t.Fatalf("baseline changed after failure: %+v", got)
	}
	writeSession(t, 12, "ppp0", "alice@example.com", "80", "160", true)
	_, clients, err := m.CollectTrafficChecked()
	if err != nil || len(clients) != 1 || clients[0].Up != 30 || clients[0].Down != 60 {
		t.Fatalf("retry delta: %+v err=%v", clients, err)
	}
}

func TestCollectTrafficCheckedRejectsIncompleteSessionMarkers(t *testing.T) {
	setupTrafficFilesystem(t)
	writeSession(t, 13, "ppp0", "alice@example.com", "80", "160", true)
	cmd := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	p := &Process{ipsecCmd: cmd, xl2tpdCmd: cmd}
	baseline := map[string]interfaceCounter{"ppp0\x00alice@example.com": {rx: 50, tx: 100}}
	m := &Manager{id: 13, proc: &managed{proc: p, tag: "tag", last: baseline}}
	path := filepath.Join(sessionDirForID(13), "ppp0")
	for _, content := range []string{"", "alice@example.com", " \n"} {
		if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, _, err := m.CollectTrafficChecked(); err == nil {
			t.Fatalf("accepted incomplete marker %q", content)
		}
		if got := baseline["ppp0\x00alice@example.com"]; got.rx != 50 || got.tx != 100 {
			t.Fatalf("baseline changed after incomplete marker %q: %+v", content, got)
		}
	}
	if err := os.WriteFile(path, []byte("alice@example.com\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, clients, err := m.CollectTrafficChecked()
	if err != nil || len(clients) != 1 || clients[0].Up != 30 || clients[0].Down != 60 {
		t.Fatalf("retry delta: %+v err=%v", clients, err)
	}
}

func TestReadSessionsFailsClosedOnUnreadableMarker(t *testing.T) {
	setupTrafficFilesystem(t)
	path := filepath.Join(sessionDirForID(14), "ppp0")
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink deterministically simulates a marker disappearing
	// between the directory scan and open, even when tests run as root.
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, ok := readSessions(14); ok || got != nil {
		t.Fatalf("unreadable marker was ignored: %#v %v", got, ok)
	}
}

func TestReadSessionsFailsClosedOnInterfaceLookupError(t *testing.T) {
	_, sysroot := setupTrafficFilesystem(t)
	writeSession(t, 15, "ppp0", "alice@example.com", "0", "0", false)
	if err := os.MkdirAll(filepath.Dir(sysroot), 0750); err != nil {
		t.Fatal(err)
	}
	// A non-directory parent produces ENOTDIR, not a known-absent interface.
	if err := os.WriteFile(sysroot, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok := readSessions(15); ok || got != nil {
		t.Fatalf("interface lookup error was ignored: %#v %v", got, ok)
	}
}

func TestCollectTrafficRejectsDisappearedSampleWithoutAdvancingBaselines(t *testing.T) {
	for _, missing := range []string{"marker", "interface"} {
		t.Run(missing, func(t *testing.T) {
			setupTrafficFilesystem(t)
			writeSession(t, 16, "ppp0", "alice@example.com", "150", "250", true)
			writeSession(t, 16, "ppp1", "bob@example.com", "80", "90", true)
			cmd := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
			p := &Process{ipsecCmd: cmd, xl2tpdCmd: cmd}
			last := map[string]interfaceCounter{
				"ppp0\x00alice@example.com": {rx: 100, tx: 200},
				"ppp1\x00bob@example.com":   {rx: 50, tx: 60},
			}
			m := &Manager{id: 16, proc: &managed{proc: p, tag: "tag", last: last}}
			path := filepath.Join(sessionDirForID(16), "ppp1")
			if missing == "interface" {
				path = filepath.Join(l2tpSysClassNetRoot, "ppp1")
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			inbounds, clients, err := m.CollectTrafficChecked()
			var disappeared *SessionDisappeared
			if !errors.As(err, &disappeared) || disappeared.InboundID != 16 || disappeared.Interface != "ppp1" || disappeared.Email != "bob@example.com" || disappeared.PreviousRx != 50 || disappeared.PreviousTx != 60 {
				t.Fatalf("wrong missing observation: %+v %v", disappeared, err)
			}
			if len(inbounds) != 0 || len(clients) != 0 || last["ppp0\x00alice@example.com"].rx != 100 || last["ppp1\x00bob@example.com"].rx != 50 {
				t.Fatalf("partial sample advanced: %#v %#v %#v", inbounds, clients, last)
			}
		})
	}
}

func TestCollectTrafficRejectsCounterDecreaseBeforeAdvancingAnyBaseline(t *testing.T) {
	setupTrafficFilesystem(t)
	writeSession(t, 16, "ppp0", "alice@example.com", "150", "250", true)
	writeSession(t, 16, "ppp1", "bob@example.com", "20", "10", true)
	cmd := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
	p := &Process{ipsecCmd: cmd, xl2tpdCmd: cmd}
	last := map[string]interfaceCounter{
		"ppp0\x00alice@example.com": {rx: 100, tx: 200},
		"ppp1\x00bob@example.com":   {rx: 30, tx: 5},
	}
	m := &Manager{id: 16, proc: &managed{proc: p, tag: "tag", last: last}}
	inbounds, clients, err := m.CollectTrafficChecked()
	var decrease *CounterDecrease
	if !errors.As(err, &decrease) || decrease.InboundID != 16 || decrease.Interface != "ppp1" || decrease.Email != "bob@example.com" || decrease.PreviousRx != 30 || decrease.ObservedRx != 20 {
		t.Fatalf("wrong observation: %+v %v", decrease, err)
	}
	if len(inbounds) != 0 || len(clients) != 0 || last["ppp0\x00alice@example.com"].rx != 100 || last["ppp1\x00bob@example.com"].rx != 30 {
		t.Fatalf("partial sample advanced: %#v %#v %#v", inbounds, clients, last)
	}
}
