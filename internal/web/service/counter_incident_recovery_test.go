package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestLocalXrayRestartRecoveryArchivesEvidenceAfterNewEpoch(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	incident := model.ResellerNodeCounterIncident{
		NodeId:    0,
		Email:     "reboot-recovery@example.test",
		Direction: "downlink",
		CreatedAt: time.Now().Add(-time.Minute).UnixMilli(),
	}
	if err := db.Create(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if err := storeCounterIncidentFallback(incident); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(counterIncidentDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("fallback entries=%v err=%v", entries, err)
	}
	old := time.Now().Add(-time.Minute)
	path := filepath.Join(counterIncidentDir(), entries[0].Name())
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	present, eligible, err := localXrayCounterIncidentRecoveryEligible(db, time.Now())
	if err != nil || !present || !eligible {
		t.Fatalf("preflight present=%v eligible=%v err=%v", present, eligible, err)
	}
	present, recovered, backupDir, err := recoverLocalXrayCounterIncidentsAfterRestart(db, time.Now())
	if err != nil || !present || !recovered || backupDir == "" {
		t.Fatalf("recovery present=%v recovered=%v backup=%q err=%v", present, recovered, backupDir, err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "counter-incident-fallback", entries[0].Name())); err != nil {
		t.Fatalf("fallback was not archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backupDir, "counter-incidents.json")); err != nil {
		t.Fatalf("database evidence was not archived: %v", err)
	}
	if err := checkLocalXrayCounterIncidents(db); err != nil {
		t.Fatalf("archived old incident still blocked the new core: %v", err)
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", 0).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("live incident rows=%d err=%v", count, err)
	}
}

func TestLocalXrayRestartRecoveryRefusesCurrentEpochEvidence(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	incident := model.ResellerNodeCounterIncident{NodeId: 0, Email: "current-epoch@example.test", Direction: "uplink"}
	if err := db.Create(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if err := storeCounterIncidentFallback(incident); err != nil {
		t.Fatal(err)
	}
	present, eligible, err := localXrayCounterIncidentRecoveryEligible(db, time.Now().Add(-time.Minute))
	if err != nil || !present || eligible {
		t.Fatalf("preflight present=%v eligible=%v err=%v", present, eligible, err)
	}
	present, recovered, backupDir, err := recoverLocalXrayCounterIncidentsAfterRestart(db, time.Now().Add(-time.Minute))
	if err != nil || !present || recovered || backupDir != "" {
		t.Fatalf("recovery present=%v recovered=%v backup=%q err=%v", present, recovered, backupDir, err)
	}
	if err := checkLocalXrayCounterIncidents(db); err == nil {
		t.Fatal("current-epoch incident was released")
	}
}
