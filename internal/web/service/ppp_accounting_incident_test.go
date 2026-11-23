package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestPPPCounterDecreasePersistsEvidenceAndBlocksOtherPanel(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Create(&model.PPPClientTrafficCounter{SessionID: "uncertain-ppp", Email: client.Email, InboundID: 42, RxBytes: 100, TxBytes: 200}).Error; err != nil {
		t.Fatal(err)
	}
	originalFolder := os.Getenv("XUI_DB_FOLDER")
	_, _, err := (&InboundService{}).AddTraffic(nil, []*xray.ClientTraffic{{Email: client.Email, PPPSessionID: "uncertain-ppp", PPPInboundID: 42, PPPRxBytes: 10, PPPTxBytes: 205}})
	var decrease *PPPCounterDecrease
	if !errors.As(err, &decrease) || decrease.PreviousRx != 100 || decrease.ObservedRx != 10 {
		t.Fatalf("counter decrease not reported: %+v %v", decrease, err)
	}
	var checkpoint model.PPPClientTrafficCounter
	if err := db.Where("session_id = ?", "uncertain-ppp").Take(&checkpoint).Error; err != nil || checkpoint.RxBytes != 100 || checkpoint.TxBytes != 200 {
		t.Fatalf("checkpoint changed: %+v %v", checkpoint, err)
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -3).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("shared incident missing: %d %v", count, err)
	}
	dir := filepath.Join(originalFolder, pppEvidenceDir)
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("evidence not saved: %v %v", files, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil || !strings.Contains(string(data), decrease.Error()) {
		t.Fatalf("evidence missing observation: %q %v", data, err)
	}
	if status := readCounterIncidentStatus(db); !status.Available || !status.FallbackPresent || status.DatabaseCount != 1 {
		t.Fatalf("dashboard missing incident: %+v", status)
	}
	// The other panel has no local evidence but reads the shared database.
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil || !strings.Contains(err.Error(), "shared PPP") {
		t.Fatalf("quota not blocked: %v", err)
	}
	if _, err := (&ClientService{}).Delete(nil, client.Id, false); err == nil {
		t.Fatal("client deletion was not blocked")
	}
	if unlock, err := LockResellerAllocation(); err != nil {
		t.Fatalf("unrelated poll blocked: %v", err)
	} else {
		unlock()
	}
	if err := db.Where("node_id = ?", -3).Delete(&model.ResellerNodeCounterIncident{}).Error; err != nil {
		t.Fatal(err)
	}
	// Even if a shared record is removed, evidence on the origin remains.
	t.Setenv("XUI_DB_FOLDER", originalFolder)
	if err := checkPPPAccountingIncident(db); err == nil {
		t.Fatal("local evidence silently cleared")
	}
}

func TestPPPIncidentRetainsEvidenceWhenDatabaseWriteFails(t *testing.T) {
	seedResellerRenewal(t)
	db := database.GetDB()
	t.Cleanup(func() { pppIncidentPending.Store(false) })
	if err := db.Exec("CREATE TRIGGER reject_ppp_incident BEFORE INSERT ON reseller_node_counter_incidents BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	decrease := &PPPCounterDecrease{SessionID: "retry", Email: "alice", InboundID: 1, PreviousRx: 100, ObservedRx: 10}
	if err := recordPPPCounterDecrease(decrease, false); err == nil || !pppIncidentPending.Load() {
		t.Fatalf("failed write not retained: %v", err)
	}
	if err := localPPPAccountingIncident(); err == nil {
		t.Fatal("failed incident did not block locally")
	}
	if err := db.Exec("DROP TRIGGER reject_ppp_incident").Error; err != nil {
		t.Fatal(err)
	}
	if err := recordPPPCounterDecrease(decrease, false); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if pppIncidentPending.Load() {
		t.Fatal("successful retry left pending latch")
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -3).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("retry did not publish: %d %v", count, err)
	}
}

func TestPPPIncidentRetainsSharedBarrierWhenLocalWriteFails(t *testing.T) {
	seedResellerRenewal(t)
	db := database.GetDB()
	t.Cleanup(func() { pppIncidentPending.Store(false) })
	path := filepath.Join(os.Getenv("XUI_DB_FOLDER"), pppEvidenceDir)
	if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	decrease := &PPPCounterDecrease{SessionID: "fail-file", Email: "alice", InboundID: 1, PreviousRx: 100, ObservedRx: 10}
	if err := recordPPPCounterDecrease(decrease, false); err == nil || !pppIncidentPending.Load() {
		t.Fatalf("storage failure not retained: %v", err)
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -3).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("shared barrier not committed: %d %v", count, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := recordPPPCounterDecrease(decrease, false); err != nil {
		t.Fatalf("local retry failed: %v", err)
	}
}

func TestPPPConcurrentIncidentSuccessDoesNotClearAnotherFailure(t *testing.T) {
	first, second := "test-first-pending", "test-second-pending"
	notePPPIncidentWrite(first, false, false)
	notePPPIncidentWrite(second, false, false)
	notePPPIncidentWrite(first, true, true)
	notePPPIncidentWrite(second, true, false)
	if !pppIncidentPending.Load() {
		t.Fatal("unrelated successful incident cleared failed observation")
	}
	notePPPIncidentWrite(first, false, false)
	notePPPIncidentWrite(first, true, false)
	if pppIncidentPending.Load() {
		t.Fatal("successful retry did not clear pending observation")
	}
}

func TestPPPOrphanBlocksQuotaAndLeavesEvidence(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	orphan := filepath.Join(bin, "pppvpn", "pptp", "712")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil || !strings.Contains(err.Error(), "PPP") {
		t.Fatalf("orphan quota guard: %v", err)
	}
	if _, err := (&ClientService{}).Delete(nil, client.Id, false); err == nil {
		t.Fatal("orphan deletion not blocked")
	}
	if status := readCounterIncidentStatus(database.GetDB()); !status.Available || !status.FallbackPresent {
		t.Fatalf("dashboard missing PPP orphan: %+v", status)
	}
	if err := os.RemoveAll(orphan); err != nil {
		t.Fatal(err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("disappearance cleared incident")
	}
	entries, err := os.ReadDir(filepath.Join(os.Getenv("XUI_DB_FOLDER"), pppEvidenceDir))
	if err != nil || len(entries) == 0 {
		t.Fatalf("missing local evidence: %v", err)
	}
}

func TestPPPHookIncidentBlocksQuotaAndSurvivesFileRemoval(t *testing.T) {
	reseller, _ := seedResellerRenewal(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	dir := filepath.Join(bin, "pppvpn", "sessions", "pptp-1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hook-error.123")
	if err := os.WriteFile(path, []byte("uncertain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("hook incident did not block quota")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("hook incident removal bypassed evidence")
	}
}
