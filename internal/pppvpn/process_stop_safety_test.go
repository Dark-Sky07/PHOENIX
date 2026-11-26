package pppvpn

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The test deliberately withholds the Wait signal even when the OS child
// exits. A signal/Kill call alone must not be reported as observed shutdown.
func TestStopRequiresObservedDaemonExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses sleep")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	oldGrace, oldForce := pppGracefulStopTimeout, pppForceStopTimeout
	pppGracefulStopTimeout, pppForceStopTimeout = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { pppGracefulStopTimeout, pppForceStopTimeout = oldGrace, oldForce })
	p := &Process{cmd: cmd, done: make(chan struct{})}
	if err := p.Stop(); err == nil || !strings.Contains(err.Error(), "exit was not observed") {
		t.Fatalf("unobserved exit reported success: %v", err)
	}
}

func TestStopRejectsMissingWaitSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses sleep")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := (&Process{cmd: cmd}).Stop(); err == nil {
		t.Fatal("missing wait signal reported success")
	}
}

func TestEnsureKeepsOldDaemonWhenStopUncertain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses sleep")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	oldGrace, oldForce := pppGracefulStopTimeout, pppForceStopTimeout
	pppGracefulStopTimeout, pppForceStopTimeout = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { pppGracefulStopTimeout, pppForceStopTimeout = oldGrace, oldForce })
	i := Instance{ID: 888, Protocol: PPTP, Port: 1723, LocalIP: "10.253.0.1", PoolCIDR: "10.253.0.0/24", DNS1: "1.1.1.1", DNS2: "8.8.8.8"}
	old := &managed{instance: i, fingerprint: "different", process: &Process{cmd: cmd, done: make(chan struct{})}}
	m := &Manager{inbounds: map[int]*managed{i.ID: old}}
	err := m.Ensure(i)
	if err == nil || !strings.Contains(err.Error(), "exit was not observed") {
		t.Fatalf("uncertain stop did not block replacement: %v", err)
	}
	if m.inbounds[i.ID] != old {
		t.Fatal("old daemon tracking was discarded")
	}
}

func TestRemoveCheckedKeepsDaemonWhenStopUncertain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses sleep")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	oldGrace, oldForce := pppGracefulStopTimeout, pppForceStopTimeout
	pppGracefulStopTimeout, pppForceStopTimeout = 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { pppGracefulStopTimeout, pppForceStopTimeout = oldGrace, oldForce })
	i := Instance{ID: 889, Protocol: PPTP}
	old := &managed{instance: i, process: &Process{cmd: cmd, done: make(chan struct{})}}
	m := &Manager{inbounds: map[int]*managed{i.ID: old}}
	if err := m.RemoveChecked(i.ID); err == nil {
		t.Fatal("uncertain stop reported successful removal")
	}
	if m.inbounds[i.ID] != old {
		t.Fatal("old daemon tracking was discarded")
	}
}
