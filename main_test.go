package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/config"
)

func TestMigrationQuiescenceChecksLocalEvidence(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	src := filepath.Join(t.TempDir(), "source.db")
	if err := checkMigrationQuiescence(src); err != nil {
		t.Fatalf("empty source and no local evidence must be quiescent: %v", err)
	}
	records := filepath.Join(config.GetBinFolderPath(), "openvpn", "750", "session-evidence", "records")
	if err := os.MkdirAll(records, 0o700); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(records, strings.Repeat("a", 32)+".open")
	if err := os.WriteFile(open, []byte("session="+strings.Repeat("a", 32)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkMigrationQuiescence(src); err == nil {
		t.Fatal("unfinished OpenVPN evidence did not block migration")
	}
}
