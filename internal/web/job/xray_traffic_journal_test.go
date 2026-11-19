package job

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

func TestXrayJournalSurvivesDatabaseFailureAndReplaysOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	client := model.ClientRecord{Email: "xray-journal@example.test", Enable: true}
	row := xray.ClientTraffic{Email: client.Email, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	batch := &xrayTrafficBatch{id: uuid.NewString(), clients: []*xray.ClientTraffic{{Email: row.Email, Up: 123, Down: 456}}}
	if err := db.Exec("CREATE TRIGGER reject_xray_journal BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	j := NewXrayTrafficJob()
	if _, _, err := j.applyJournaledXrayBatch(batch); err == nil {
		t.Fatal("expected injected failure")
	}
	path := filepath.Join(xrayJournalDir(), batch.id+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal lost: %v", err)
	}
	if err := db.Exec("DROP TRIGGER reject_xray_journal").Error; err != nil {
		t.Fatal(err)
	}
	// Simulate a commit followed by a crash before removing the journal.
	if _, _, err := j.inboundService.AddXrayTrafficBatch(batch.id, nil, batch.clients); err != nil {
		t.Fatal(err)
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("id = ?", row.Id).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 123 || row.Down != 456 {
		t.Fatalf("replayed batch counted incorrectly: %d/%d", row.Up, row.Down)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("journal not removed: %v", err)
	}
	var count int64
	if err := db.Model(&model.XrayTrafficReceipt{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("receipt cleanup = %d, %v", count, err)
	}
}

func TestXrayJournalRejectsInvalidBatchID(t *testing.T) {
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := storeXrayBatch(&xrayTrafficBatch{id: "../escape"}); err == nil {
		t.Fatal("invalid batch ID accepted")
	}
}

func TestXrayJournalRejectsReusedEmailInsteadOfBillingReplacement(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	client := model.ClientRecord{Email: "reused@example.test", Enable: true}
	old := xray.ClientTraffic{Email: client.Email, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	batch := &xrayTrafficBatch{id: uuid.NewString(), clients: []*xray.ClientTraffic{{Email: client.Email, Up: 123}}}
	j := NewXrayTrafficJob()
	var err error
	batch.identities, err = j.inboundService.CaptureXrayTrafficIdentities(batch.clients)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeXrayBatch(batch); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&client).Error; err != nil {
		t.Fatal(err)
	}
	replacement := model.ClientRecord{Email: client.Email, Enable: true}
	replacementTraffic := xray.ClientTraffic{Email: client.Email, Enable: true}
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&replacementTraffic).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err == nil {
		t.Fatal("reused email should require reconciliation")
	}
	if err := db.First(&replacementTraffic, replacementTraffic.Id).Error; err != nil {
		t.Fatal(err)
	}
	if replacementTraffic.Up != 0 {
		t.Fatal("old traffic charged to replacement account")
	}
	if _, err := os.Stat(filepath.Join(xrayJournalDir(), batch.id+".json")); err != nil {
		t.Fatalf("unreconciled journal lost: %v", err)
	}
}

func TestXrayJournalRejectsOwnershipChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	client := model.ClientRecord{Email: "transfer@example.test", Enable: true}
	row := xray.ClientTraffic{Email: client.Email, Enable: true}
	owner := model.ResellerClient{Email: client.Email, ResellerId: 1}
	for _, v := range []any{&client, &row, &owner} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	j := NewXrayTrafficJob()
	batch := &xrayTrafficBatch{id: uuid.NewString(), clients: []*xray.ClientTraffic{{Email: client.Email, Down: 99}}}
	var err error
	batch.identities, err = j.inboundService.CaptureXrayTrafficIdentities(batch.clients)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeXrayBatch(batch); err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&owner).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerClient{Email: client.Email, ResellerId: 2}).Error; err != nil {
		t.Fatal(err)
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err == nil {
		t.Fatal("ownership change should require reconciliation")
	}
	if err := db.First(&row, row.Id).Error; err != nil {
		t.Fatal(err)
	}
	if row.Down != 0 {
		t.Fatal("old owner's traffic charged to new owner")
	}
}

func TestXrayJournalLateTrafficSurvivesClientDeletion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	reseller := model.Reseller{Username: "journal-owner", Password: "hash", Enable: true, TrafficLimit: 2000}
	client := model.ClientRecord{Email: "late-delete@example.test", Enable: true, TotalGB: 1000}
	counter := xray.ClientTraffic{Email: client.Email, Enable: true, Up: 40, Down: 60}
	for _, v := range []any{&reseller, &client, &counter} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.ResellerClient{Email: client.Email, ResellerId: reseller.Id}).Error; err != nil {
		t.Fatal(err)
	}
	j := NewXrayTrafficJob()
	batch := &xrayTrafficBatch{id: uuid.NewString(), clients: []*xray.ClientTraffic{{Email: client.Email, Up: 123, Down: 456}}}
	var err error
	batch.identities, err = j.inboundService.CaptureXrayTrafficIdentities(batch.clients)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeXrayBatch(batch); err != nil {
		t.Fatal(err)
	}
	// Use the real deletion path, which archives the already persisted bytes.
	if _, err := (&service.ClientService{}).Delete(&service.InboundService{}, client.Id, false); err != nil {
		t.Fatal(err)
	}
	replacement := model.ClientRecord{Email: client.Email, Enable: true}
	replacementCounter := xray.ClientTraffic{Email: client.Email, Enable: true}
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&replacementCounter).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER fail_late_archive BEFORE UPDATE OF prior_consumed_bytes ON reseller_traffic_archives BEGIN SELECT RAISE(ABORT, 'injected archive failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err == nil {
		t.Fatal("archive failure must keep the batch for retry")
	}
	var receipts int64
	if err := db.Model(&model.XrayTrafficReceipt{}).Count(&receipts).Error; err != nil || receipts != 0 {
		t.Fatalf("failed archive committed receipt: %d %v", receipts, err)
	}
	if err := db.Exec("DROP TRIGGER fail_late_archive").Error; err != nil {
		t.Fatal(err)
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&replacementCounter, replacementCounter.Id).Error; err != nil {
		t.Fatal(err)
	}
	if replacementCounter.Up != 0 || replacementCounter.Down != 0 {
		t.Fatalf("replacement billed old traffic: %+v", replacementCounter)
	}
	stat, err := (&service.ResellerService{}).Stat(&reseller)
	if err != nil || stat.UsedTraffic != 679 || stat.AllocatedTraffic != 679 {
		t.Fatalf("late traffic was not archived: %+v %v", stat, err)
	}
}
