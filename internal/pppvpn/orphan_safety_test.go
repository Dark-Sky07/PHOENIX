package pppvpn

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestUntrackedPPPRuntimeIsNotSignalledOrDeleted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix signals")
	}
	root := setupTrafficTest(t)
	inst := Instance{ID: 888, Protocol: PPTP, Port: 1723, LocalIP: "10.253.0.1", PoolCIDR: "10.253.0.0/24", DNS1: "1.1.1.1", DNS2: "8.8.8.8"}
	dir := filepath.Join(root, "pptp", "888")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(dir, "pptpd.conf")
	if err := os.WriteFile(evidence, []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	m := &Manager{}
	for _, err := range []error{m.CheckUntrackedRuntime(), m.Ensure(inst), m.RemoveChecked(inst.ID)} {
		if err == nil || !strings.Contains(err.Error(), "manual review") {
			t.Fatalf("untracked runtime not rejected: %v", err)
		}
	}
	m.Reconcile([]Instance{inst})
	if err := m.Ensure(inst); err == nil {
		t.Fatal("error placeholder bypassed orphan guard")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("untracked process signalled: %v", err)
	}
	if b, err := os.ReadFile(evidence); err != nil || string(b) != "evidence" {
		t.Fatalf("evidence altered: %q %v", b, err)
	}
}

func TestPPPOrphanScanExemptsOnlyOwnedCanonicalRuntime(t *testing.T) {
	root := setupTrafficTest(t)
	if err := os.MkdirAll(filepath.Join(root, "pptp", "7"), 0700); err != nil {
		t.Fatal(err)
	}
	m := &Manager{inbounds: map[int]*managed{7: {instance: Instance{ID: 7, Protocol: PPTP}, process: &Process{}}}}
	if err := m.CheckUntrackedRuntime(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pptp", "007"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckUntrackedRuntime(); err == nil {
		t.Fatal("noncanonical runtime ignored")
	}
	if err := os.RemoveAll(filepath.Join(root, "pptp")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pptp"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckUntrackedRuntime(); err == nil {
		t.Fatal("uninspectable runtime ignored")
	}
}
