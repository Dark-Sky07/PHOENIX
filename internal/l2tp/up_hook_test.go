package l2tp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSuccessfulUpHookClearsPendingMarker(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell and sync")
	}
	setupTrafficFilesystem(t)
	inst := validInstance()
	script := filepath.Join(t.TempDir(), "ip-up")
	if err := os.WriteFile(script, []byte(renderIPUpScript(inst)), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = []string{"IFNAME=lo", "PEERNAME=alice@example.test"}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("up hook failed: %v: %s", err, output)
	}
	if err := CheckHookIncidents(); err != nil {
		t.Fatalf("completed up hook left pending evidence: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(sessionDirForID(inst.Id), "lo")); err != nil || string(data) != "alice@example.test\n" {
		t.Fatalf("session marker missing: %q %v", data, err)
	}
}

func TestKilledUpHookLeavesPendingEvidence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process state")
	}
	setupTrafficFilesystem(t)
	inst := validInstance()
	content := renderIPUpScript(inst)
	needle := "sync \"$pending\" \"$incidents\"\n"
	if strings.Count(content, needle) != 1 {
		t.Fatal("pending sync location not found")
	}
	content = strings.Replace(content, needle, needle+"kill -STOP $$\n", 1)
	script := filepath.Join(t.TempDir(), "ip-up")
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Env = []string{"IFNAME=lo", "PEERNAME=alice@example.test"}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	defer func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(cmd.Process.Pid), "status"))
		if err == nil && strings.Contains(string(data), "State:\tT") {
			stopped = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("hook did not pause after syncing pending evidence")
	}
	var pending *HookPendingError
	if err := CheckHookIncidents(); !errors.As(err, &pending) {
		t.Fatalf("live pending hook not observed: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	finished = true
	if err := CheckHookIncidents(); !errors.As(err, &pending) {
		t.Fatalf("interrupted hook lost pending evidence: %v", err)
	}
	if _, err := os.Stat(pending.Path); err != nil {
		t.Fatalf("pending evidence removed: %v", err)
	}
}

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
