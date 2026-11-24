package openvpn

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStartRetainsUnverifiedPIDEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	tempBinFolder(t)
	bin := filepath.Join(openvpnBinDir(), "openvpn")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "openvpn.pid")
	// A live PID, even this panel's own PID, must not be signalled.
	original := fmt.Sprintf("%d\n", os.Getpid())
	if err := os.WriteFile(pidFile, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	p := newProcess(filepath.Join(dir, "openvpn.conf"), "test")
	if err := p.Start(); err == nil || !strings.Contains(err.Error(), "manual reconciliation") {
		t.Fatalf("retained PID accepted: %v", err)
	}
	got, err := os.ReadFile(pidFile)
	if err != nil || string(got) != original {
		t.Fatalf("PID evidence changed: %q %v", got, err)
	}
}

func TestStopRemovesOnlyConfirmedOwnedPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	for _, mismatch := range []bool{false, true} {
		t.Run(strconv.FormatBool(mismatch), func(t *testing.T) {
			tempBinFolder(t)
			bin := filepath.Join(openvpnBinDir(), "openvpn")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$$\" > \"$(dirname \"$2\")/openvpn.pid\"\nexec sleep 60\n"), 0700); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			p := newProcess(filepath.Join(dir, "openvpn.conf"), "test")
			if err := p.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if p.cmd != nil && p.cmd.Process != nil {
					_ = p.cmd.Process.Kill()
					<-p.done
				}
			})
			pidFile := filepath.Join(dir, "openvpn.pid")
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(pidFile); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("fixture did not write PID")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mismatch {
				if err := os.WriteFile(pidFile, []byte("999999\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := p.Stop()
			if mismatch {
				if err == nil {
					t.Fatal("mismatched PID accepted")
				}
				if data, e := os.ReadFile(pidFile); e != nil || string(data) != "999999\n" {
					t.Fatalf("mismatched evidence lost: %q %v", data, e)
				}
			} else if err != nil {
				t.Fatalf("owned stop failed: %v", err)
			} else if _, err := os.Lstat(pidFile); !os.IsNotExist(err) {
				t.Fatalf("owned PID not removed: %v", err)
			}
		})
	}
}

func TestRemoveDataDirRetainsPIDEvidence(t *testing.T) {
	tempBinFolder(t)
	const id = 91
	if err := os.MkdirAll(dataDirForID(id), 0750); err != nil {
		t.Fatal(err)
	}
	pid := pidPathForID(id)
	if err := os.WriteFile(pid, []byte("not-a-pid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeDataDir(id); err == nil {
		t.Fatal("removed unverified PID evidence")
	}
	if data, err := os.ReadFile(pid); err != nil || string(data) != "not-a-pid\n" {
		t.Fatalf("evidence lost: %q %v", data, err)
	}
}
