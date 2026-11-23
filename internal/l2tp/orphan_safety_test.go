package l2tp

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestUntrackedL2TPRuntimeIsNotSignalledOrDeleted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix process signal")
	}
	setupTrafficFilesystem(t)
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	inst := validInstance()
	if err := os.MkdirAll(sessionDirForID(inst.Id), 0750); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(sessionDirForID(inst.Id), "ppp0")
	if err := os.WriteFile(marker, []byte("client@example.com\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(xl2tpdPIDPath(inst.Id), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	m := &Manager{}
	assertRetained := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "manual review") {
			t.Fatalf("orphan cleanup was not blocked: %v", err)
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("untracked process was signalled: %v", err)
		}
		if b, err := os.ReadFile(marker); err != nil || string(b) != "client@example.com\n" {
			t.Fatalf("session evidence changed: %q, %v", b, err)
		}
	}
	assertRetained(m.Ensure(inst))
	if !strings.Contains(m.Status().Error, "manual review") {
		t.Fatal("ensure did not report orphan in runtime status")
	}
	assertRetained(m.RemoveChecked(inst.Id))
	assertRetained(m.StopManually())
	m.Reconcile(nil)
	assertRetained(m.Restart())
	m.Reconcile([]Instance{inst})
	if !strings.Contains(m.Status().Error, "manual review") {
		t.Fatal("reconcile did not report orphan")
	}
}

func TestOrphanScanFailsClosedAndExemptsOnlyTrackedCanonicalDirectory(t *testing.T) {
	root, _ := setupTrafficFilesystem(t)
	if err := os.MkdirAll(dataDirForID(7), 0750); err != nil {
		t.Fatal(err)
	}
	if err := orphanDataError(7); err != nil {
		t.Fatalf("tracked runtime rejected: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "007"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := orphanDataError(7); err == nil {
		t.Fatal("noncanonical runtime ignored")
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := orphanDataError(0); err == nil {
		t.Fatal("unreadable runtime root treated as empty")
	}
}

func TestCollectTrafficPreservesBaselinesOnUntrackedRuntime(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		t.Run(strconv.FormatBool(tracked), func(t *testing.T) {
			setupTrafficFilesystem(t)
			writeSession(t, 8, "ppp8", "orphan@example.test", "12", "34", true)
			m := &Manager{}
			if tracked {
				writeSession(t, 7, "ppp7", "tracked@example.test", "100", "200", true)
				cmd := &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}
				m.id = 7
				m.proc = &managed{proc: &Process{ipsecCmd: cmd, xl2tpdCmd: cmd}, last: map[string]interfaceCounter{"ppp7\x00tracked@example.test": {rx: 50, tx: 70}}}
			}
			_, _, err := m.CollectTrafficChecked()
			var failure *SessionReadFailure
			if !errors.As(err, &failure) || !strings.Contains(failure.Cause, "untracked L2TP") {
				t.Fatalf("untracked runtime ignored: %v", err)
			}
			if tracked && (len(failure.Previous) != 1 || m.proc.last["ppp7\x00tracked@example.test"].rx != 50) {
				t.Fatalf("baseline lost or advanced: %+v", failure.Previous)
			}
			if _, err := os.Stat(filepath.Join(sessionDirForID(8), "ppp8")); err != nil {
				t.Fatalf("orphan evidence changed: %v", err)
			}
		})
	}
}

func TestTrackedL2TPIsNotReplacedByDifferentInbound(t *testing.T) {
	setupTrafficFilesystem(t)
	inst := validInstance()
	m := &Manager{id: inst.Id + 1, proc: &managed{proc: &Process{}}}
	if err := m.Ensure(inst); err == nil || !strings.Contains(err.Error(), "remove tracked") {
		t.Fatalf("replacement was not blocked: %v", err)
	}
	if m.id != inst.Id+1 || m.proc == nil {
		t.Fatal("tracked state discarded")
	}
}
