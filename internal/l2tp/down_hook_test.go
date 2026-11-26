package l2tp

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDownHookPersistsIncidentAndRetainsUnpolledSession(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux sysfs and sync")
	}
	for _, name := range []string{"sync", "mktemp", "head"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}
	root, _ := setupTrafficFilesystem(t)
	inst := validInstance()
	marker := filepath.Join(sessionDirForID(inst.Id), "lo")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("alice@example.test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "ip-down")
	if err := os.WriteFile(script, []byte(renderIPDownScript(inst)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = []string{"IFNAME=lo", "PEERNAME=alice@example.test"}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("down hook failed: %v: %s", err, output)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "alice@example.test\n" {
		t.Fatalf("down hook removed or changed marker evidence: %q %v", data, err)
	}
	dir := filepath.Join(root, "hook-incidents")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("missing durable hook evidence: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil || !strings.Contains(string(data), "alice@example.test") || !strings.Contains(string(data), "statistics/rx_bytes:") {
		t.Fatalf("incomplete down-hook evidence: %q %v", data, err)
	}
	var incident *HookIncidentError
	if err := CheckHookIncidents(); !errors.As(err, &incident) {
		t.Fatalf("hook incident not visible: %v", err)
	}
	m := &Manager{id: inst.Id, proc: &managed{}}
	if err := m.CheckUntrackedRuntime(); !errors.As(err, &incident) {
		t.Fatalf("tracked runtime hid hook incident: %v", err)
	}
	if _, _, err := readSessions(inst.Id); !errors.As(err, &incident) {
		t.Fatalf("collector ignored hook incident: %v", err)
	}
	if err := CheckClientSessionEvidence([]string{"unrelated"}, nil); !errors.As(err, &incident) {
		t.Fatalf("removal ignored unknown hook evidence: %v", err)
	}
}

func TestInvalidDownHookStillLeavesIncident(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell and sync")
	}
	setupTrafficFilesystem(t)
	inst := validInstance()
	script := filepath.Join(t.TempDir(), "ip-down")
	if err := os.WriteFile(script, []byte(renderIPDownScript(inst)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = []string{"IFNAME=../bad", "PEERNAME=alice"}
	if err := cmd.Run(); err == nil {
		t.Fatal("invalid interface was accepted")
	}
	var incident *HookIncidentError
	if err := CheckHookIncidents(); !errors.As(err, &incident) {
		t.Fatalf("invalid hook left no incident: %v", err)
	}
}

func TestDownHookFailureRetainsActiveMarker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell")
	}
	root, _ := setupTrafficFilesystem(t)
	inst := validInstance()
	marker := filepath.Join(sessionDirForID(inst.Id), "lo")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("alice\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Prevent creation of the incident directory without relying on permissions.
	if err := os.WriteFile(filepath.Join(root, "hook-incidents"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "ip-down")
	if err := os.WriteFile(script, []byte(renderIPDownScript(inst)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = []string{"IFNAME=lo", "PEERNAME=alice"}
	if err := cmd.Run(); err == nil {
		t.Fatal("incident creation failure reported success")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "alice\n" {
		t.Fatalf("active evidence lost after failed incident write: %q %v", data, err)
	}
}
