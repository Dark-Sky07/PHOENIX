//go:build linux

package l2tp

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

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
