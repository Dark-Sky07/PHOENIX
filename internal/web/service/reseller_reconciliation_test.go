package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func seedResellerWithDanglingMapping(t *testing.T) (model.Reseller, model.ResellerClient) {
	t.Helper()
	setupConflictDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "reconcile-test", Password: "hash", Enable: true, TrafficLimit: 1000}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	mapping := model.ResellerClient{ResellerId: r.Id, Email: "dangling@example.test"}
	if err := db.Create(&mapping).Error; err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy mapping whose client record was removed without
	// cleaning the ownership row. No client record is created here.
	return r, mapping
}

func TestResellerReadOnlyViewsDegradeDanglingMapping(t *testing.T) {
	r, _ := seedResellerWithDanglingMapping(t)
	svc := &ResellerService{}
	if _, err := svc.Stat(&r); err == nil {
		t.Fatal("strict Stat accepted a dangling reseller client mapping")
	}
	stats, err := svc.List()
	if err != nil {
		t.Fatalf("degraded list failed: %v", err)
	}
	if len(stats) != 1 || len(stats[0].Warnings) == 0 {
		t.Fatalf("degraded list lost warning: %+v", stats)
	}
	if !strings.Contains(stats[0].Warnings[0], "no client record") {
		t.Fatalf("unexpected warning: %q", stats[0].Warnings[0])
	}
	csv := string(RenderResellerListCSV(stats))
	if !strings.Contains(csv, ",warnings\r\n") || !strings.Contains(csv, "no client record") {
		t.Fatalf("CSV did not carry reconciliation warning: %q", csv)
	}
}

func TestResellerReadOnlyViewsDegradeCounterIncident(t *testing.T) {
	r, _ := seedResellerWithDanglingMapping(t)
	db := database.GetDB()
	if err := db.Where("email = ?", "dangling@example.test").Delete(&model.ResellerClient{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerNodeCounterIncident{NodeId: 0, Direction: "core-exited"}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ResellerService{}
	if _, err := svc.Stat(&r); err == nil {
		t.Fatal("strict Stat accepted a core-exit incident")
	}
	stats, err := svc.List()
	if err != nil {
		t.Fatalf("degraded list failed: %v", err)
	}
	if len(stats) != 1 || len(stats[0].Warnings) == 0 {
		t.Fatalf("degraded list lost incident warning: %+v", stats)
	}
}

func TestReconcileCounterIncidentsBacksUpBeforeClearing(t *testing.T) {
	seedResellerWithDanglingMapping(t)
	db := database.GetDB()
	incident := model.ResellerNodeCounterIncident{NodeId: 0, Direction: "core-exited"}
	if err := db.Create(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if err := storeCounterIncidentFallback(incident); err != nil {
		t.Fatal(err)
	}
	if err := BeginXrayPoll("reconcile-pending"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xrayCoreExitPending.Store(false); xrayCounterIncidentPending.Store(false) })

	report := ReadReconciliationReport(db)
	if len(report.DanglingMappings) != 1 || len(report.DBIncidents) != 1 || len(report.InFlightPolls) != 1 || len(report.FallbackFiles) != 1 {
		t.Fatalf("incomplete reconciliation report: %+v", report)
	}

	dry, err := ReconcileCounterIncidents(db, ReconcileOptions{Confirm: ReconcileConfirmToken, DryRun: true})
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	if !dry.DryRun || dry.BackupDir != "" {
		t.Fatalf("dry run changed state: %+v", dry)
	}
	var stillMapped int64
	if err := db.Model(&model.ResellerClient{}).Count(&stillMapped).Error; err != nil || stillMapped != 1 {
		t.Fatalf("dry run deleted mapping: count=%d err=%v", stillMapped, err)
	}

	// A wrong token must never mutate evidence, even when DryRun is false.
	if _, err := ReconcileCounterIncidents(db, ReconcileOptions{Confirm: "wrong", DryRun: false}); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ResellerClient{}).Count(&stillMapped).Error; err != nil || stillMapped != 1 {
		t.Fatalf("wrong token deleted mapping: count=%d err=%v", stillMapped, err)
	}

	result, err := ReconcileCounterIncidents(db, ReconcileOptions{Confirm: ReconcileConfirmToken, DryRun: false})
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if result.DryRun || result.BackupDir == "" || result.ArchivedMappings != 1 || result.ClearedIncidents != 1 || result.ClearedPolls != 1 || result.MovedFallbackFiles != 1 {
		t.Fatalf("unexpected repair result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(result.BackupDir, "report-before.json")); err != nil {
		t.Fatalf("backup report missing: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(result.BackupDir, "report-before.json"))
	if err != nil {
		t.Fatal(err)
	}
	var backedUp ReconciliationReport
	if err := json.Unmarshal(data, &backedUp); err != nil {
		t.Fatalf("invalid backup report: %v", err)
	}
	if len(backedUp.DanglingMappings) != 1 || len(backedUp.DBIncidents) != 1 {
		t.Fatalf("backup lost evidence: %+v", backedUp)
	}

	var archived int64
	if err := db.Model(&model.ResellerClientMappingArchive{}).Count(&archived).Error; err != nil || archived != 1 {
		t.Fatalf("mapping not archived: count=%d err=%v", archived, err)
	}
	if err := db.Model(&model.ResellerClient{}).Count(&stillMapped).Error; err != nil || stillMapped != 0 {
		t.Fatalf("dangling mapping retained: count=%d err=%v", stillMapped, err)
	}
	var incidents, polls int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Count(&incidents).Error; err != nil || incidents != 0 {
		t.Fatalf("incident retained: count=%d err=%v", incidents, err)
	}
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&polls).Error; err != nil || polls != 0 {
		t.Fatalf("poll marker retained: count=%d err=%v", polls, err)
	}
	fallback, err := listDirectoryEntries(counterIncidentDir())
	if err != nil || len(fallback) != 0 {
		t.Fatalf("fallback evidence retained: %v err=%v", fallback, err)
	}
	if _, err := os.Stat(filepath.Join(config.GetDBFolderPath(), "reconciliation-backups")); err != nil {
		t.Fatalf("backup root missing: %v", err)
	}
}

func TestClientDeletionArchivesResellerMapping(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	db := database.GetDB()
	var before int64
	if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ?", reseller.Id).Count(&before).Error; err != nil || before != 1 {
		t.Fatalf("seed mapping missing: count=%d err=%v", before, err)
	}
	if _, err := (&ClientService{}).DeleteByEmail(&InboundService{}, client.Email, false); err != nil {
		t.Fatalf("delete client failed: %v", err)
	}
	var after int64
	if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ?", reseller.Id).Count(&after).Error; err != nil || after != 0 {
		t.Fatalf("mapping not removed: count=%d err=%v", after, err)
	}
	var archived int64
	if err := db.Model(&model.ResellerClientMappingArchive{}).
		Where("reseller_id = ? AND LOWER(TRIM(email)) = LOWER(?)", reseller.Id, client.Email).
		Count(&archived).Error; err != nil || archived != 1 {
		t.Fatalf("mapping not archived: count=%d err=%v", archived, err)
	}
}
