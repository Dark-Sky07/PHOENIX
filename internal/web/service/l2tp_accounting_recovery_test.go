package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestL2TPRestartRecoveryRejectsCurrentDaemonDatabaseEvidence(t *testing.T) {
	startedAt := time.Now()
	if !l2tpIncidentsPredateDaemon([]model.ResellerNodeCounterIncident{{CreatedAt: startedAt.Add(-time.Second).UnixMilli()}}, startedAt) {
		t.Fatal("pre-daemon database incident was rejected")
	}
	if !l2tpIncidentsPredateDaemon([]model.ResellerNodeCounterIncident{{CreatedAt: 0}}, startedAt) {
		t.Fatal("legacy database incident without a timestamp was rejected")
	}
	if l2tpIncidentsPredateDaemon([]model.ResellerNodeCounterIncident{{CreatedAt: startedAt.UnixMilli()}}, startedAt) {
		t.Fatal("current daemon database incident became recovery eligible")
	}
	if l2tpIncidentsPredateDaemon([]model.ResellerNodeCounterIncident{{CreatedAt: startedAt.Add(time.Millisecond).UnixMilli()}}, startedAt) {
		t.Fatal("later database incident became recovery eligible")
	}
}

func TestL2TPRecoveryRejectsSymlinkEvidence(t *testing.T) {
	setupConflictDB(t)
	evidenceDir := filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-evidence")
	if err := os.MkdirAll(evidenceDir, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(os.Getenv("XUI_DB_FOLDER"), "outside-evidence.txt")
	if err := os.WriteFile(target, []byte("evidence\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(evidenceDir, "not-a-real-evidence.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := l2tpRecoveryFiles(evidenceDir); err == nil {
		t.Fatal("symlink evidence was accepted")
	}
}

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
