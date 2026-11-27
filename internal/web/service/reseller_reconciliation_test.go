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
	l2tpIncident := model.ResellerNodeCounterIncident{NodeId: -2, Direction: "l2tp-orphan"}
	if err := db.Create(&l2tpIncident).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l2tpAccountingIncidentPath(), []byte("old L2TP incident\n"), 0600); err != nil {
		t.Fatal(err)
	}
	l2tpEvidenceDir := filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-evidence")
	if err := os.MkdirAll(l2tpEvidenceDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l2tpEvidenceDir, "old-l2tp-evidence.txt"), []byte("old L2TP observation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := BeginXrayPoll("reconcile-pending"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { xrayCoreExitPending.Store(false); xrayCounterIncidentPending.Store(false) })

	report := ReadReconciliationReport(db)
	if len(report.DanglingMappings) != 1 || len(report.DBIncidents) != 2 || len(report.InFlightPolls) != 1 || len(report.FallbackFiles) != 1 || !report.L2TPMarkerPresent || len(report.L2TPEvidenceFiles) != 1 {
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
	if result.DryRun || result.BackupDir == "" || result.ArchivedMappings != 1 || result.ClearedIncidents != 2 || result.ClearedPolls != 1 || result.MovedFallbackFiles != 1 || !result.MovedL2TPMarker || result.MovedL2TPEvidenceFiles != 1 {
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
	if len(backedUp.DanglingMappings) != 1 || len(backedUp.DBIncidents) != 2 || !backedUp.L2TPMarkerPresent || len(backedUp.L2TPEvidenceFiles) != 1 {
		t.Fatalf("backup lost evidence: %+v", backedUp)
	}
	if _, err := os.Stat(filepath.Join(result.BackupDir, "l2tp-accounting-incident")); err != nil {
		t.Fatalf("L2TP marker was not archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(result.BackupDir, "l2tp-accounting-evidence", "old-l2tp-evidence.txt")); err != nil {
		t.Fatalf("L2TP evidence was not archived: %v", err)
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
	if _, err := os.Stat(l2tpAccountingIncidentPath()); !os.IsNotExist(err) {
		t.Fatalf("L2TP marker retained after reconciliation: %v", err)
	}
	l2tpMarker, l2tpEvidence, err := listL2TPReconciliationEvidence()
	if err != nil {
		t.Fatalf("read L2TP evidence after reconciliation: %v", err)
	}
	if l2tpMarker || len(l2tpEvidence) != 0 {
		t.Fatalf("L2TP evidence remained after reconciliation: marker=%v files=%v", l2tpMarker, l2tpEvidence)
	}
	if _, err := os.Stat(filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-evidence", "old-l2tp-evidence.txt")); !os.IsNotExist(err) {
		t.Fatalf("L2TP evidence retained after reconciliation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(config.GetDBFolderPath(), "reconciliation-backups")); err != nil {
		t.Fatalf("backup root missing: %v", err)
	}
}

func TestReconcileCounterIncidentsRetainsDatabaseBarrierWhenL2TPEvidenceChanges(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	if err := db.Where("node_id = ? AND email = ?", -2, "").FirstOrCreate(&model.ResellerNodeCounterIncident{NodeId: -2, Direction: "l2tp-orphan"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l2tpAccountingIncidentPath(), []byte("old L2TP incident\n"), 0600); err != nil {
		t.Fatal(err)
	}
	evidenceDir := filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-evidence")
	if err := os.MkdirAll(evidenceDir, 0700); err != nil {
		t.Fatal(err)
	}
	oldEvidence := filepath.Join(evidenceDir, "old-l2tp-evidence.txt")
	if err := os.WriteFile(oldEvidence, []byte("old L2TP observation\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reconciliationAfterBackupHook = nil })
	reconciliationAfterBackupHook = func() {
		if err := os.WriteFile(filepath.Join(evidenceDir, "new-l2tp-evidence.txt"), []byte("new L2TP observation\n"), 0600); err != nil {
			t.Fatalf("inject fresh L2TP evidence: %v", err)
		}
	}

	result, err := ReconcileCounterIncidents(db, ReconcileOptions{Confirm: ReconcileConfirmToken, DryRun: false})
	if err == nil || !strings.Contains(err.Error(), "L2TP accounting evidence changed") {
		t.Fatalf("fresh L2TP evidence did not stop reconciliation: result=%+v err=%v", result, err)
	}
	if result.BackupDir == "" {
		t.Fatalf("old evidence was not backed up before the safe refusal: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(result.BackupDir, "l2tp-accounting-evidence", "old-l2tp-evidence.txt")); err != nil {
		t.Fatalf("old L2TP evidence was not archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(evidenceDir, "new-l2tp-evidence.txt")); err != nil {
		t.Fatalf("fresh L2TP evidence was not retained: %v", err)
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("L2TP database barrier was cleared after a new observation: count=%d err=%v", count, err)
	}
}

func TestReconcileCounterIncidentsPreservesEvidenceCreatedAfterItsBackup(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	xrayCoreExitPending.Store(false)
	xrayCounterIncidentPending.Store(false)
	xrayAPIOutagePending.Store(false)
	t.Cleanup(func() {
		reconciliationAfterBackupHook = nil
		xrayCoreExitPending.Store(false)
		xrayCounterIncidentPending.Store(false)
		xrayAPIOutagePending.Store(false)
	})

	oldIncident := model.ResellerNodeCounterIncident{NodeId: 0, Email: "", Direction: "core-exited"}
	if err := db.Create(&oldIncident).Error; err != nil {
		t.Fatal(err)
	}
	if err := BeginXrayPoll("reconcile-old-poll"); err != nil {
		t.Fatal(err)
	}
	var hookErr error
	reconciliationAfterBackupHook = func() {
		if err := db.Create(&model.ResellerNodeCounterIncident{NodeId: -2, Email: "", Direction: "l2tp-session-disappeared"}).Error; err != nil {
			hookErr = err
			return
		}
		hookErr = BeginXrayPoll("reconcile-new-poll")
	}

	result, err := ReconcileCounterIncidents(db, ReconcileOptions{Confirm: ReconcileConfirmToken, DryRun: false})
	if err != nil {
		t.Fatal(err)
	}
	if hookErr != nil {
		t.Fatalf("could not inject later evidence: %v", hookErr)
	}
	if result.ClearedIncidents != 1 || result.ClearedPolls != 1 {
		t.Fatalf("repair cleared a different snapshot than expected: %+v", result)
	}
	var incidents []model.ResellerNodeCounterIncident
	if err := db.Order("id ASC").Find(&incidents).Error; err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || incidents[0].Direction != "l2tp-session-disappeared" {
		t.Fatalf("new counter evidence was deleted: %+v", incidents)
	}
	var polls []model.XrayTrafficInFlight
	if err := db.Order("id ASC").Find(&polls).Error; err != nil {
		t.Fatal(err)
	}
	if len(polls) != 1 || polls[0].ID != "reconcile-new-poll" {
		t.Fatalf("new poll marker was deleted: %+v", polls)
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
