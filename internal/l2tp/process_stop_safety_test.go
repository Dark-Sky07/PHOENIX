package l2tp

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func unobservedL2TPProcess(t *testing.T, missing bool) *Process {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses sleep")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	oldGrace, oldForce := gracefulStopTimeout, forceStopTimeout
	gracefulStopTimeout, forceStopTimeout = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { gracefulStopTimeout, forceStopTimeout = oldGrace, oldForce })
	var done chan struct{}
	if !missing {
		done = make(chan struct{})
	}
	return &Process{xl2tpdCmd: cmd, xl2tpdDone: done}
}

func TestStopRequiresObservedL2TPDaemonExit(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "unobserved", true: "missing-wait"}[missing], func(t *testing.T) {
			p := unobservedL2TPProcess(t, missing)
			if err := p.Stop(); err == nil {
				t.Fatal("uncertain daemon exit reported successful stop")
			}
		})
	}
}

func TestEnsureAndRemoveRetainL2TPStateWhenStopUncertain(t *testing.T) {
	setupTrafficFilesystem(t)
	p := unobservedL2TPProcess(t, false)
	inst := validInstance()
	old := &managed{proc: p, fingerprint: "old", last: map[string]interfaceCounter{"session": {rx: 42}}}
	m := &Manager{proc: old, id: inst.Id, ready: true}
	path := filepath.Join(dataDirForID(inst.Id), "evidence")
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	check := func(err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "exit was not observed") {
			t.Fatalf("uncertain stop reported success: %v", err)
		}
		if m.proc != old || m.id != inst.Id || old.last["session"].rx != 42 {
			t.Fatal("lost tracked process or baseline")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("lost runtime evidence: %v", err)
		}
	}
	check(m.Ensure(inst))
	check(m.RemoveChecked(inst.Id))
	check(m.StopManually())
	m.Reconcile(nil)
	if m.proc != old {
		t.Fatal("periodic reconciliation discarded uncertain daemon")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("periodic reconciliation discarded evidence: %v", err)
	}
	check(m.Restart())
}

func TestStopObservesBothL2TPDaemons(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses sleep")
	}
	start := func() (*exec.Cmd, chan struct{}) {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		return cmd, done
	}
	a, ad := start()
	b, bd := start()
	p := &Process{ipsecCmd: a, ipsecDone: ad, xl2tpdCmd: b, xl2tpdDone: bd}
	if err := p.Stop(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ad:
	default:
		t.Fatal("strongSwan wait not observed")
	}
	select {
	case <-bd:
	default:
		t.Fatal("xl2tpd wait not observed")
	}
}
