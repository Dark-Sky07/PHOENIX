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

func TestConcurrentUpHooksNeverReplaceSessionMarker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell and sync")
	}
	setupTrafficFilesystem(t)
	inst := validInstance()
	script := filepath.Join(t.TempDir(), "ip-up")
	if err := os.WriteFile(script, []byte(renderIPUpScript(inst)), 0700); err != nil {
		t.Fatal(err)
	}
	alice := exec.Command(script)
	alice.Env = []string{"IFNAME=lo", "PEERNAME=alice@example.test"}
	bob := exec.Command(script)
	bob.Env = []string{"IFNAME=lo", "PEERNAME=bob@example.test"}
	if err := alice.Start(); err != nil {
		t.Fatal(err)
	}
	if err := bob.Start(); err != nil {
		_ = alice.Process.Kill()
		_ = alice.Wait()
		t.Fatal(err)
	}
	aliceErr, bobErr := alice.Wait(), bob.Wait()
	if (aliceErr == nil) == (bobErr == nil) {
		t.Fatalf("expected one successful marker and one fenced collision: alice=%v bob=%v", aliceErr, bobErr)
	}
	winner, loser := "alice@example.test", "bob@example.test"
	if bobErr == nil {
		winner, loser = loser, winner
	}
	marker := filepath.Join(sessionDirForID(inst.Id), "lo")
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != winner+"\n" {
		t.Fatalf("session marker overwritten: %q %v", data, err)
	}
	var incident *HookIncidentError
	if err := CheckHookIncidents(); !errors.As(err, &incident) {
		t.Fatalf("collision did not leave incident: %v", err)
	}
	data, err = os.ReadFile(incident.Path)
	if err != nil || !strings.Contains(string(data), loser) {
		t.Fatalf("collision lost attempted identity: %q %v", data, err)
	}
}

func TestInvalidUpHookLeavesIncident(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell and sync")
	}
	setupTrafficFilesystem(t)
	script := filepath.Join(t.TempDir(), "ip-up")
	if err := os.WriteFile(script, []byte(renderIPUpScript(validInstance())), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = []string{"IFNAME=../bad", "PEERNAME=alice"}
	if err := cmd.Run(); err == nil {
		t.Fatal("invalid interface accepted")
	}
	var incident *HookIncidentError
	if err := CheckHookIncidents(); !errors.As(err, &incident) {
		t.Fatalf("invalid up hook left no incident: %v", err)
	}
}

func TestLateDownHookDoesNotRemoveDifferentSessionMarker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell and sync")
	}
	setupTrafficFilesystem(t)
	inst := validInstance()
	marker := filepath.Join(sessionDirForID(inst.Id), "lo")
	if err := os.MkdirAll(filepath.Dir(marker), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("bob@example.test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "ip-down")
	if err := os.WriteFile(script, []byte(renderIPDownScript(inst)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = []string{"IFNAME=lo", "PEERNAME=alice@example.test"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("down hook: %v: %s", err, out)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "bob@example.test\n" {
		t.Fatalf("late hook erased different marker: %q %v", data, err)
	}
	var incident *HookIncidentError
	if err := CheckHookIncidents(); !errors.As(err, &incident) {
		t.Fatalf("late hook did not leave incident: %v", err)
	}
	data, err = os.ReadFile(incident.Path)
	if err != nil || !strings.Contains(string(data), "alice@example.test") || !strings.Contains(string(data), "bob@example.test") {
		t.Fatalf("conflicting identities not retained: %q %v", data, err)
	}
}
