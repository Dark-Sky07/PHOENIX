package l2tp

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type incidentTestEntry struct {
	name string
	info fs.FileInfo
	err  error
}

func (e incidentTestEntry) Name() string               { return e.name }
func (e incidentTestEntry) IsDir() bool                { return e.info != nil && e.info.IsDir() }
func (e incidentTestEntry) Type() fs.FileMode          { return 0 }
func (e incidentTestEntry) Info() (fs.FileInfo, error) { return e.info, e.err }

func TestHookIncidentPendingDisappearsDuringScan(t *testing.T) {
	root, _ := setupTrafficFilesystem(t)
	entries := []os.DirEntry{incidentTestEntry{name: "pending.abc", err: os.ErrNotExist}}
	var pending *HookPendingError
	if err := classifyHookIncidentEntries(entries); !errors.As(err, &pending) || pending.Path != filepath.Join(root, "hook-incidents", "pending.abc") {
		t.Fatalf("removed pending evidence did not defer this scan: %v", err)
	}
	entries = append(entries, incidentTestEntry{name: "up.failure"})
	var incident *HookIncidentError
	if err := classifyHookIncidentEntries(entries); !errors.As(err, &incident) {
		t.Fatalf("pending disappearance concealed a permanent incident: %v", err)
	}
	entries = []os.DirEntry{incidentTestEntry{name: "pending.abc", err: os.ErrPermission}}
	if err := classifyHookIncidentEntries(entries); !errors.Is(err, os.ErrPermission) || errors.As(err, &pending) {
		t.Fatalf("uninspectable evidence was treated as transient: %v", err)
	}
}

func TestClientSessionEvidenceChecksL2TPMarkersWithoutInterface(t *testing.T) {
	setupTrafficFilesystem(t)
	writeSession(t, 7, "ppp0", "Alice@example.test", "0", "0", false)
	if err := CheckClientSessionEvidence([]string{"  alice@EXAMPLE.test "}, nil); err == nil {
		t.Fatal("offline or stale matching marker ignored")
	}
	if err := CheckClientSessionEvidence([]string{"bob@example.test"}, nil); err != nil {
		t.Fatalf("unrelated marker blocked: %v", err)
	}
	if err := CheckClientSessionEvidence([]string{"alice@example.test"}, []int{8}); err != nil {
		t.Fatalf("unrelated scope blocked: %v", err)
	}
	if err := CheckClientSessionEvidence([]string{"alice@example.test"}, []int{}); err != nil {
		t.Fatalf("empty scope blocked: %v", err)
	}
}

func TestClientSessionEvidenceFailsClosedOnInvalidMarkers(t *testing.T) {
	for _, content := range []string{"", "alice", "alice\nextra\n", " \n", "alice\tbob\n", "\xff\n", strings.Repeat("a", 4097) + "\n"} {
		t.Run(content[:min(len(content), 12)], func(t *testing.T) {
			setupTrafficFilesystem(t)
			path := filepath.Join(sessionDirForID(7), "ppp0")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if err := CheckClientSessionEvidence([]string{"other@example.test"}, nil); err == nil {
				t.Fatal("malformed identity treated as unrelated")
			}
			if _, _, err := readSessions(7); err == nil {
				t.Fatal("collector accepted malformed identity")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("marker evidence removed: %v", err)
			}
		})
	}
}

func TestClientSessionEvidenceRejectsSymlinksAndInvalidRuntimeEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary")
	}
	t.Run("record", func(t *testing.T) {
		setupTrafficFilesystem(t)
		dir := sessionDirForID(7)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "marker")
		if err := os.WriteFile(outside, []byte("alice\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dir, "ppp0")); err != nil {
			t.Fatal(err)
		}
		if err := CheckClientSessionEvidence([]string{"bob"}, nil); err == nil {
			t.Fatal("session symlink accepted")
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "alice\n" {
			t.Fatalf("outside evidence altered: %q %v", data, err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		setupTrafficFilesystem(t)
		if err := os.MkdirAll(dataDirForID(7), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(t.TempDir(), sessionDirForID(7)); err != nil {
			t.Fatal(err)
		}
		if err := CheckClientSessionEvidence([]string{"alice"}, nil); err == nil {
			t.Fatal("session directory symlink accepted")
		}
	})
	t.Run("noncanonical inbound", func(t *testing.T) {
		root, _ := setupTrafficFilesystem(t)
		if err := os.MkdirAll(filepath.Join(root, "007"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := CheckClientSessionEvidence([]string{"alice"}, nil); err == nil {
			t.Fatal("noncanonical runtime entry ignored")
		}
	})
}
