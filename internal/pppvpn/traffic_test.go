package pppvpn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func setupTrafficTest(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin with spaces'and quote")
	t.Setenv("XUI_BIN_FOLDER", bin)
	trafficCollector = pppTrafficCollector{}
	t.Cleanup(func() { trafficCollector = pppTrafficCollector{} })
	return filepath.Join(bin, "pppvpn")
}

func TestCollectTrafficRetriesAndAcknowledgesClosedSession(t *testing.T) {
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, "ppp4.closed.session-uuid")
	if err := os.WriteFile(session, []byte("closed\tppp4\tAlice@example.test\t1200\t2500\tsession-uuid\t7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := CollectTraffic()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Clients) != 1 {
		t.Fatalf("got %d client records, want one", len(first.Clients))
	}
	client := first.Clients[0]
	if client.Email != "Alice@example.test" || client.PPPSessionID != "session-uuid" || client.PPPInboundID != 7 || client.PPPRxBytes != 1200 || client.PPPTxBytes != 2500 || len(first.ClosedSessionIDs) != 1 || first.ClosedSessionIDs[0] != "session-uuid" {
		t.Fatalf("unexpected absolute PPP counters: %+v", client)
	}
	retry, err := CollectTraffic()
	if err != nil {
		t.Fatal(err)
	}
	if retry.Token != first.Token || len(retry.Clients) != 1 || retry.Clients[0].PPPRxBytes != 1200 || retry.Clients[0].PPPTxBytes != 2500 {
		t.Fatalf("retry did not preserve the exact batch: %#v vs %#v", retry, first)
	}
	if err := AckTraffic(first.Token + 1); err == nil {
		t.Fatal("expected stale traffic acknowledgement to fail")
	}
	if err := AckTraffic(first.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(session); !os.IsNotExist(err) {
		t.Fatalf("completed session record was not removed: %v", err)
	}
	next, err := CollectTraffic()
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Clients) != 0 {
		t.Fatalf("closed session was collected again after acknowledgement: %#v", next.Clients)
	}
}

func TestTrafficHookAndPPPDOptionsWireUpAndDownAccounting(t *testing.T) {
	root := setupTrafficTest(t)
	i := Instance{ID: 9, Protocol: PPTP}
	options := renderPPPDOptions(i)
	if !strings.Contains(options, "ip-up-script "+filepath.Join(dirFor(i), "ip-up")) || !strings.Contains(options, "ip-down-script "+filepath.Join(dirFor(i), "ip-down")) {
		t.Fatalf("PPP options do not configure both accounting hooks:\n%s", options)
	}
	sessionDir := filepath.Join(root, "sessions", "pptp-9")
	up := renderTrafficHook(sessionDir, i.ID, false)
	down := renderTrafficHook(sessionDir, i.ID, true)
	for name, script := range map[string]string{"ip-up": up, "ip-down": down} {
		if strings.Contains(script, "%!") {
			t.Errorf("%s hook has an unexpanded format placeholder:\n%s", name, script)
		}
		if !strings.Contains(script, "PEERNAME") || !strings.Contains(script, "rx_bytes") || !strings.Contains(script, "tx_bytes") || !strings.Contains(script, "session_id") {
			t.Errorf("%s hook is missing user/counter/session handling:\n%s", name, script)
		}
		if strings.Contains(script, "PEERNAME:-${2:-}") {
			t.Errorf("%s hook must not mistake pppd's second positional argument (TTY) for a username:\n%s", name, script)
		}
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("sh", "-n", path).CombinedOutput(); err != nil {
			t.Errorf("%s hook has invalid shell syntax: %v\n%s", name, err, output)
		}
	}
	if !strings.Contains(up, "/proc/sys/kernel/random/uuid") {
		t.Fatal("new PPP sessions must have unique durable checkpoint identities")
	}
	if !strings.Contains(down, "cut -f6") || !strings.Contains(down, "closed") {
		t.Fatal("ip-down must preserve the active session identity and final counters")
	}
}
