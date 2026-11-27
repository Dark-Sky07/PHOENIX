package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestL2TPRestartRecoveryRequiresEvidenceOlderThanDaemonEpoch(t *testing.T) {
	setupConflictDB(t)
	marker := l2tpAccountingIncidentPath()
	if err := os.WriteFile(marker, []byte("old incident\n"), 0600); err != nil {
		t.Fatal(err)
	}
	evidenceDir := filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-evidence")
	if err := os.MkdirAll(evidenceDir, 0700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(evidenceDir, "old.txt")
	if err := os.WriteFile(evidence, []byte("old observation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	for _, path := range []string{marker, evidence} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	eligible, err := l2tpRestartRecoveryEligible(time.Now())
	if err != nil || !eligible {
		t.Fatalf("old reboot evidence was not eligible: eligible=%v err=%v", eligible, err)
	}

	now := time.Now()
	if err := os.Chtimes(evidence, now, now); err != nil {
		t.Fatal(err)
	}
	eligible, err = l2tpRestartRecoveryEligible(now.Add(-time.Second))
	if err != nil || eligible {
		t.Fatalf("current daemon evidence became eligible: eligible=%v err=%v", eligible, err)
	}
}
