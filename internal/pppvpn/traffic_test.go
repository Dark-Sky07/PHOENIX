package pppvpn

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestCollectTrafficRetainsUnfinishedAndMalformedSessions(t *testing.T) {
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "ppp0.closed.good")
	if err := os.WriteFile(good, []byte("closed\tppp0\talice\t10\t20\tgood\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(dir, "ppp1.closed.second.tmp.123")
	if err := os.WriteFile(temp, []byte("closed\tppp1\tbob\t30\t40\tsecond\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("unfinished hook file accepted")
	}
	for _, path := range []string{temp, good} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("session evidence lost: %s: %v", path, err)
		}
	}
	if err := os.Remove(temp); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "ppp1.closed.bad")
	if err := os.WriteFile(bad, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("malformed session accepted")
	}
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	batch, err := CollectTraffic()
	if err != nil || len(batch.Clients) != 1 {
		t.Fatalf("retry lost good session: %+v %v", batch, err)
	}
	if err := AckTraffic(batch.Token); err != nil {
		t.Fatal(err)
	}
}

func TestCollectTrafficRejectsUninspectableSessionRoot(t *testing.T) {
	root := setupTrafficTest(t)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sessions"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("uninspectable session spool reported empty")
	}
}

func TestAckTrafficRetainsReplacementAndRepolls(t *testing.T) {
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ppp0.closed.session")
	if err := os.WriteFile(path, []byte("closed\tppp0\talice\t10\t20\tsession\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := CollectTraffic()
	if err != nil {
		t.Fatal(err)
	}
	// Like ip-down, replace the file atomically after the old sample was read.
	tmp := filepath.Join(dir, "replacement")
	if err := os.WriteFile(tmp, []byte("closed\tppp0\talice\t30\t40\tsession\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := AckTraffic(batch.Token); err == nil {
		t.Fatal("changed session was acknowledged")
	}
	// Simulate panel restart: claimed records must be rediscovered without memory.
	trafficCollector = pppTrafficCollector{}
	retry, err := CollectTraffic()
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Clients) != 1 || retry.Clients[0].PPPRxBytes != 30 || retry.Clients[0].PPPTxBytes != 40 {
		t.Fatalf("replacement not recovered: %+v", retry.Clients)
	}
	if err := AckTraffic(retry.Token); err != nil {
		t.Fatal(err)
	}
	if next, err := CollectTraffic(); err != nil || len(next.Clients) != 0 {
		t.Fatalf("recovered claim left an incomplete directory: %+v %v", next, err)
	}
}

func TestEmptySessionClaimRequiresReconciliation(t *testing.T) {
	root := setupTrafficTest(t)
	claim := filepath.Join(root, "sessions-staged", "claim-interrupted")
	if err := os.MkdirAll(claim, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTraffic(); err == nil || !strings.Contains(err.Error(), "empty session claim") {
		t.Fatalf("empty claim accepted by collector: %v", err)
	}
	if err := CheckClientSessionEvidence([]string{"alice"}, nil); err == nil || !strings.Contains(err.Error(), "empty session claim") {
		t.Fatalf("empty claim accepted by removal check: %v", err)
	}
	if _, err := os.Stat(claim); err != nil {
		t.Fatalf("claim evidence was removed: %v", err)
	}
}

func TestAckMissingSourceLeavesBlockingClaim(t *testing.T) {
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ppp0.closed.session")
	if err := os.WriteFile(path, []byte("closed\tppp0\talice\t10\t20\tsession\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := CollectTraffic()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := AckTraffic(batch.Token); err == nil {
		t.Fatal("missing source was acknowledged")
	}
	trafficCollector = pppTrafficCollector{}
	if _, err := CollectTraffic(); err == nil || !strings.Contains(err.Error(), "empty session claim") {
		t.Fatalf("missing source evidence ignored after restart: %v", err)
	}
}

func TestAckTrafficRetainsMalformedReplacement(t *testing.T) {
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ppp0.closed.session")
	if err := os.WriteFile(path, []byte("closed\tppp0\talice\t10\t20\tsession\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := CollectTraffic()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := AckTraffic(batch.Token); err == nil {
		t.Fatal("malformed replacement acknowledged")
	}
	trafficCollector = pppTrafficCollector{}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("malformed claim ignored after restart")
	}
}

func TestAckTrafficRejectsActiveReplacementInClaim(t *testing.T) {
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ppp0.closed.session")
	if err := os.WriteFile(path, []byte("closed\tppp0\talice\t10\t20\tsession\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	batch, err := CollectTraffic()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("active\tlo\talice\t0\t0\tnew-session\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := AckTraffic(batch.Token); err == nil {
		t.Fatal("active replacement acknowledged")
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("active staged record silently treated as accounted")
	}
}

func TestReadPPPSessionRejectsExtraRecords(t *testing.T) {
	root := setupTrafficTest(t)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "record")
	if err := os.WriteFile(path, []byte("closed\tppp0\talice\t10\t20\tsession\t7\nextra\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPPPSession(path); err == nil {
		t.Fatal("extra record ignored")
	}
}

func TestTrafficHooksFenceInterfaceReuseAndOwnerMismatch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux PPP hooks")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed")
	}
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	up, down := filepath.Join(t.TempDir(), "up"), filepath.Join(t.TempDir(), "down")
	for path, script := range map[string]string{up: renderTrafficHook(dir, 7, false), down: renderTrafficHook(dir, 7, true)} {
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	run := func(script string) error {
		cmd := exec.Command(script)
		cmd.Env = append(os.Environ(), "IFNAME=lo", "PEERNAME=alice", fmt.Sprintf("PPPD_PID=%d", os.Getpid()))
		return cmd.Run()
	}
	if err := run(up); err != nil {
		t.Fatalf("ip-up: %v", err)
	}
	active := filepath.Join(dir, "lo")
	original, err := os.ReadFile(active)
	if err != nil {
		t.Fatal(err)
	}
	if err := run(up); err == nil {
		t.Fatal("second up overwrote active session")
	}
	if got, err := os.ReadFile(active); err != nil || string(got) != string(original) {
		t.Fatalf("active replaced: %q %v", got, err)
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("collision incident was ignored")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "hook-error.") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	fields := strings.Split(strings.TrimSpace(string(original)), "\t")
	if len(fields) != 9 {
		t.Fatalf("unexpected record: %q", original)
	}
	fields[7] += "-old"
	if err := os.WriteFile(active, []byte(strings.Join(fields, "\t")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(down); err == nil {
		t.Fatal("down accepted another owner's session")
	}
	if got, err := os.ReadFile(active); err != nil || !strings.Contains(string(got), "-old") {
		t.Fatalf("mismatched active removed: %q %v", got, err)
	}
}

func TestTrafficHooksNormalSessionPreservesOwnerAndFinalCounters(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux PPP hooks")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed")
	}
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	paths := []string{filepath.Join(t.TempDir(), "up"), filepath.Join(t.TempDir(), "down")}
	for i, path := range paths {
		if err := os.WriteFile(path, []byte(renderTrafficHook(dir, 7, i == 1)), 0700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(path)
		cmd.Env = append(os.Environ(), "IFNAME=lo", "PEERNAME=alice", fmt.Sprintf("PPPD_PID=%d", os.Getpid()))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook %d: %v %s", i, err, output)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "lo")); !os.IsNotExist(err) {
		t.Fatalf("active record remained: %v", err)
	}
	batch, err := CollectTraffic()
	if err != nil || len(batch.Clients) != 1 || len(batch.ClosedSessionIDs) != 1 {
		t.Fatalf("closed session: %+v %v", batch, err)
	}
	if err := AckTraffic(batch.Token); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyActiveAndWrongInterfaceGenerationFailClosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux sysfs")
	}
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "lo")
	if err := os.WriteFile(path, []byte("active\tlo\talice\t0\t0\told\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("legacy active session silently accepted")
	}
	index, err := readCounter("/sys/class/net/lo/ifindex")
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf("active\tlo\talice\t0\t0\tnew\t7\towner\t%d\n", index+1)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("reused interface silently accepted")
	}
}

func TestKilledHookLeavesPendingMarker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux flock and hooks")
	}
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed")
	}
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	locks := filepath.Join(dir, ".hook-locks")
	if err := os.MkdirAll(locks, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(locks, "lo"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(t.TempDir(), "up")
	if err := os.WriteFile(path, []byte(renderTrafficHook(dir, 7, false)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "IFNAME=lo", "PEERNAME=alice", fmt.Sprintf("PPPD_PID=%d", os.Getpid()))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The hook may be waiting on an external command. Kill its whole group so
	// an orphan cannot continue mutating the test spool during cleanup.
	groupAlive := true
	defer func() {
		if groupAlive {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	var pending string
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "hook-pending.") {
				pending = filepath.Join(dir, entry.Name())
				break
			}
		}
		if pending != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pending == "" {
		t.Fatal("hook did not create pending marker")
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	groupAlive = false
	if _, err := os.Stat(pending); err != nil {
		t.Fatalf("killed hook lost marker: %v", err)
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("interrupted hook was treated as empty spool")
	} else {
		var busy *HookPendingError
		if !errors.As(err, &busy) {
			t.Fatalf("unexpected pending error: %v", err)
		}
	}
}
