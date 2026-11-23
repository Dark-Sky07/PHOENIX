package pppvpn

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPPPSessionRootRejectsEscapingSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary")
	}
	t.Run("session scan", func(t *testing.T) {
		root := setupTrafficTest(t)
		outside := t.TempDir()
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "sessions")); err != nil {
			t.Fatal(err)
		}
		if _, err := CollectTraffic(); err == nil {
			t.Fatal("followed outside session directory")
		}
		if b, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || string(b) != "untouched" {
			t.Fatalf("outside file altered: %q %v", b, err)
		}
	})
	t.Run("file read", func(t *testing.T) {
		root := setupTrafficTest(t)
		outside := t.TempDir()
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
		secret := filepath.Join(outside, "record")
		if err := os.WriteFile(secret, []byte("closed\tlo\talice\t1\t2\tid\t7\n"), 0600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "link")
		if err := os.Symlink(secret, link); err != nil {
			t.Fatal(err)
		}
		if _, err := readPPPSession(link); err == nil {
			t.Fatal("followed outside session file")
		}
		if _, err := readPPPSession(secret); err == nil {
			t.Fatal("accepted outside session path")
		}
	})
	t.Run("ack staging", func(t *testing.T) {
		root := setupTrafficTest(t)
		outside := t.TempDir()
		dir := filepath.Join(root, "sessions", "pptp-7")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		record := filepath.Join(dir, "ppp.closed.id")
		data := []byte("closed\tppp\talice\t1\t2\tid\t7\n")
		if err := os.WriteFile(record, data, 0600); err != nil {
			t.Fatal(err)
		}
		batch, err := CollectTraffic()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "sessions-staged")); err != nil {
			t.Fatal(err)
		}
		if err := AckTraffic(batch.Token); err == nil {
			t.Fatal("followed outside staging path")
		}
		if b, err := os.ReadFile(record); err != nil || string(b) != string(data) {
			t.Fatalf("source evidence lost: %q %v", b, err)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("outside staging modified: %v %v", entries, err)
		}
	})
}

func TestPPPConfigSpoolCreationRejectsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary")
	}
	root := setupTrafficTest(t)
	outside := t.TempDir()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sessions")); err != nil {
		t.Fatal(err)
	}
	inst := Instance{ID: 7, Protocol: PPTP, Port: 1723, LocalIP: "10.253.0.1", PoolCIDR: "10.253.0.0/24", DNS1: "1.1.1.1", DNS2: "8.8.8.8"}
	if _, err := writeConfig(inst); err == nil {
		t.Fatal("spool escaped root")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory modified: %v %v", entries, err)
	}
}
