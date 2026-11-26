//go:build linux

package pppvpn

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPPPSpoolRejectsFIFOWithoutBlocking(t *testing.T) {
	for _, relative := range []string{"sessions", "sessions-staged", "sessions/pptp-7/record", "sessions-staged/claim-1/record"} {
		t.Run(relative, func(t *testing.T) {
			root := setupTrafficTest(t)
			path := filepath.Join(root, relative)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := CollectTraffic(); err == nil {
				t.Fatal("FIFO accepted as a spool directory or record")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("FIFO evidence removed: %v", err)
			}
		})
	}
}

func TestPPPRecordRejectsInternalSymlink(t *testing.T) {
	root := setupTrafficTest(t)
	dir := filepath.Join(root, "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "record")
	if err := os.WriteFile(record, []byte("closed\tlo\talice\t1\t2\tid\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias")
	if err := os.Symlink("record", link); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectTraffic(); err == nil {
		t.Fatal("session symlink accepted as a second record")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink evidence removed: %v", err)
	}
}
