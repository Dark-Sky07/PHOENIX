//go:build linux

package l2tp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPendingHookSpecialFilesRequireManualReview(t *testing.T) {
	for _, kind := range []string{"fifo", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root, _ := setupTrafficFilesystem(t)
			dir := filepath.Join(root, "hook-incidents")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "pending.test")
			var err error
			switch kind {
			case "fifo":
				err = unix.Mkfifo(path, 0600)
			case "symlink":
				err = os.Symlink(filepath.Join(t.TempDir(), "missing"), path)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			var incident *HookIncidentError
			if err := CheckHookIncidents(); !errors.As(err, &incident) {
				t.Fatalf("%s not classified as incident: %v", kind, err)
			}
		})
	}
}

func TestClientSessionEvidenceRejectsFIFOWithoutBlocking(t *testing.T) {
	for _, relative := range []string{"7/sessions", "7/sessions/ppp0"} {
		t.Run(relative, func(t *testing.T) {
			root, _ := setupTrafficFilesystem(t)
			path := filepath.Join(root, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckClientSessionEvidence([]string{"alice"}, nil); err == nil {
				t.Fatal("FIFO accepted by removal check")
			}
			if _, _, err := readSessions(7); err == nil {
				t.Fatal("FIFO accepted by collector")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("FIFO evidence removed: %v", err)
			}
		})
	}
}
