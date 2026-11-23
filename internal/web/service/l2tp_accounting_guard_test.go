package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestL2TPOrphanBlocksQuotaAndDeletionButNotTrafficLock(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	orphan := filepath.Join(bin, "l2tp", "712")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "evidence"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := &ResellerService{}
	if err := svc.CheckTrafficQuota(&reseller, 1); err == nil || !strings.Contains(err.Error(), "L2TP accounting") {
		t.Fatalf("traffic quota not blocked: %v", err)
	}
	if err := svc.CheckClientQuota(&reseller, 1, 1); err == nil {
		t.Fatal("client quota not blocked")
	}
	if _, err := (&ClientService{}).Delete(nil, client.Id, false); err == nil {
		t.Fatal("client deletion not blocked")
	}
	if _, _, err := (&ClientService{}).BulkDelete(nil, []string{client.Email}, false); err == nil {
		t.Fatal("bulk client deletion not blocked")
	}
	tracker := &renewalQuotaTracker{tx: database.GetDB(), accounts: map[int]*renewalQuotaSnapshot{}}
	if _, err := tracker.load(reseller.Id); err == nil {
		t.Fatal("renewal quota not blocked")
	}
	if status := readCounterIncidentStatus(database.GetDB()); !status.Available || !status.FallbackPresent {
		t.Fatalf("incident not shown on dashboard: %+v", status)
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		t.Fatalf("unrelated traffic poll lock blocked: %v", err)
	}
	unlock()
	marker, err := os.ReadFile(l2tpAccountingIncidentPath())
	if err != nil || !strings.Contains(string(marker), "712") {
		t.Fatalf("incident evidence missing: %q %v", marker, err)
	}
	// Removing runtime evidence alone is not a reconciliation or release.
	if err := os.RemoveAll(orphan); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("lost durable barrier after orphan disappeared")
	}
	if err := os.Remove(l2tpAccountingIncidentPath()); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckTrafficQuota(&reseller, 1); err != nil {
		t.Fatalf("barrier remained after simulated reviewed incident: %v", err)
	}
}

func TestL2TPIncidentPersistenceFailureStaysLatched(t *testing.T) {
	seedResellerRenewal(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	dbdir := os.Getenv("XUI_DB_FOLDER")
	orphan := filepath.Join(bin, "l2tp", "713")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(bad, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XUI_DB_FOLDER", bad)
	t.Cleanup(func() { l2tpIncidentPending.Store(false) })
	if err := ObserveL2TPAccountingIncident(); err == nil || !l2tpIncidentPending.Load() {
		t.Fatalf("failed persistence not latched: %v", err)
	}
	if err := os.RemoveAll(orphan); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XUI_DB_FOLDER", dbdir)
	if err := ObserveL2TPAccountingIncident(); err == nil {
		t.Fatal("lost incident after directory disappeared")
	}
	if l2tpIncidentPending.Load() {
		t.Fatal("successfully persisted retry remained pending")
	}
	if _, err := os.Stat(l2tpAccountingIncidentPath()); err != nil {
		t.Fatalf("incident retry did not persist: %v", err)
	}
}

func TestPublishedL2TPIncidentBlocksOtherHostAndSurvivesLocalRemoval(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	firstDBFolder := os.Getenv("XUI_DB_FOLDER")
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	if err := os.MkdirAll(filepath.Join(bin, "l2tp", "714"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := PublishL2TPAccountingIncident(); err == nil {
		t.Fatal("incident reported success")
	}
	if err := PublishL2TPAccountingIncident(); err == nil {
		t.Fatal("repeated incident reported success")
	}
	db := database.GetDB()
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("shared incident not idempotent: %d %v", count, err)
	}
	// Simulate a second panel with an independent bin and fallback folder but
	// the same database. It must block sensitive paths without blocking polls.
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := checkL2TPAccountingIncident(db); err == nil || !strings.Contains(err.Error(), "shared L2TP") {
		t.Fatalf("shared incident ignored: %v", err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("other panel allocated quota")
	}
	if _, err := (&ClientService{}).Delete(nil, client.Id, false); err == nil {
		t.Fatal("other panel deleted client")
	}
	if status := readCounterIncidentStatus(db); !status.Available || status.DatabaseCount != 1 {
		t.Fatalf("shared incident missing from dashboard: %+v", status)
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		t.Fatalf("traffic lock blocked: %v", err)
	}
	unlock()
	if err := db.Where("node_id = ?", -2).Delete(&model.ResellerNodeCounterIncident{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := checkL2TPAccountingIncident(db); err != nil {
		t.Fatalf("barrier retained after simulated shared review: %v", err)
	}
	// The origin still retains its independent evidence and can publish again.
	t.Setenv("XUI_DB_FOLDER", firstDBFolder)
	if err := PublishL2TPAccountingIncident(); err == nil {
		t.Fatal("source evidence was lost")
	}
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("source did not republish: %d %v", count, err)
	}
}

func TestL2TPSharedPublicationFailureKeepsLocalEvidence(t *testing.T) {
	seedResellerRenewal(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	if err := os.MkdirAll(filepath.Join(bin, "l2tp", "715"), 0700); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_l2tp_incident BEFORE INSERT ON reseller_node_counter_incidents BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := PublishL2TPAccountingIncident(); err == nil || !strings.Contains(err.Error(), "persist shared") {
		t.Fatalf("publication failure hidden: %v", err)
	}
	if _, err := os.Stat(l2tpAccountingIncidentPath()); err != nil {
		t.Fatalf("local evidence not retained: %v", err)
	}
	if err := db.Exec("DROP TRIGGER reject_l2tp_incident").Error; err != nil {
		t.Fatal(err)
	}
	if err := PublishL2TPAccountingIncident(); err == nil {
		t.Fatal("incident should still require manual review")
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("publication retry failed: %d %v", count, err)
	}
}
