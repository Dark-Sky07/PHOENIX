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

func TestXrayShutdownFlushRetriesPendingWithoutDoubleCounting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	client := model.ClientRecord{Email: "shutdown-flush@example.test", Enable: true}
	traffic := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, row := range []any{&client, &traffic} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	job := NewXrayTrafficJob()
	job.pending = &xrayTrafficBatch{id: uuid.NewString(), clients: []*xray.ClientTraffic{{Email: client.Email, Up: 31, Down: 12}}}
	if err := db.Exec("CREATE TRIGGER fail_shutdown_flush BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := job.FlushBeforeStop(); err == nil || job.pending == nil {
		t.Fatalf("failure lost pending batch: %v", err)
	}
	if err := db.Exec("DROP TRIGGER fail_shutdown_flush").Error; err != nil {
		t.Fatal(err)
	}
	if err := job.FlushBeforeStop(); err != nil {
		t.Fatal(err)
	}
	if job.pending != nil {
		t.Fatal("successful flush retained pending batch")
	}
	if err := job.FlushBeforeStop(); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&traffic, traffic.Id).Error; err != nil || traffic.Up != 31 || traffic.Down != 12 {
		t.Fatalf("unexpected traffic: %+v %v", traffic, err)
	}
}

func TestXrayJournalRejectsUnchangedAmbiguousResellerIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		counters []string
	}{
		{name: "missing counter"},
		{name: "case variant counters", counters: []string{"ambiguous@example.test", "AMBIGUOUS@example.test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XUI_DB_FOLDER", dir)
			if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.CloseDB() })
			db := database.GetDB()
			client := model.ClientRecord{Email: "ambiguous@example.test", Enable: true}
			owner := model.ResellerClient{Email: client.Email, ResellerId: 1}
			for _, row := range []any{&client, &owner} {
				if err := db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, email := range tc.counters {
				if err := db.Create(&xray.ClientTraffic{Email: email, Enable: true}).Error; err != nil {
					t.Fatal(err)
				}
			}
			j := NewXrayTrafficJob()
			batch := &xrayTrafficBatch{id: uuid.NewString(), clients: []*xray.ClientTraffic{{Email: client.Email, Up: 123}}}
			var err error
			batch.identities, err = j.inboundService.CaptureXrayTrafficIdentities(batch.clients)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := j.applyJournaledXrayBatch(batch); err == nil {
				t.Fatal("ambiguous identity was accepted")
			}
			if _, err := os.Stat(filepath.Join(xrayJournalDir(), batch.id+".json")); err != nil {
				t.Fatalf("journal lost: %v", err)
			}
			var rows []xray.ClientTraffic
			if err := db.Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Up != 0 {
					t.Fatalf("ambiguous traffic charged: %+v", row)
				}
			}
			var receipts int64
			if err := db.Model(&model.XrayTrafficReceipt{}).Count(&receipts).Error; err != nil || receipts != 0 {
				t.Fatalf("unexpected receipt: %d %v", receipts, err)
			}
		})
	}
}

func TestInterruptedXrayPollMarkerBlocksQuotaUntilJournalReplay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	reseller := model.Reseller{Username: "poll-marker", Password: "hash", Enable: true, TrafficLimit: 1000}
	client := model.ClientRecord{Email: "poll-marker@example.test", Enable: true, TotalGB: 100}
	counter := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, v := range []any{&reseller, &client, &counter} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}).Error; err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if err := service.BeginXrayPoll(id); err != nil {
		t.Fatal(err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("orphan poll marker allowed allocation")
	}
	if err := (&service.ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("orphan poll marker allowed quota check")
	}
	batch := &xrayTrafficBatch{id: id, clients: []*xray.ClientTraffic{{Email: client.Email, Up: 123}}}
	var err error
	batch.identities, err = (&service.InboundService{}).CaptureXrayTrafficIdentities(batch.clients)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeXrayBatch(batch); err != nil {
		t.Fatal(err)
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&counter, counter.Id).Error; err != nil || counter.Up != 123 {
		t.Fatalf("recovered usage missing: %+v %v", counter, err)
	}
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatalf("replayed marker still blocks allocation: %v", err)
	}
	unlock()
}

func TestPollMarkerDeleteFailureRetainsReceiptAndRetriesOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	client := model.ClientRecord{Email: "marker-retry@example.test", Enable: true}
	counter := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, row := range []any{&client, &counter} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	j := NewXrayTrafficJob()
	t.Cleanup(func() {
		if j.captureUnlock != nil {
			j.captureUnlock()
			j.captureUnlock = nil
		}
	})
	if err := j.prepareXrayBatch(func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, []*xray.ClientTraffic{{Email: client.Email, Up: 123}}, nil
	}, j.inboundService.CaptureXrayTrafficIdentities); err != nil {
		t.Fatal(err)
	}
	id := j.pending.id
	if err := db.Exec("CREATE TRIGGER reject_poll_marker_delete BEFORE DELETE ON xray_traffic_in_flights BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.applyPending(j.applyJournaledXrayBatch); err == nil || j.pending == nil || j.captureUnlock == nil {
		t.Fatal("failed marker deletion lost the pending batch or lock")
	}
	var count int64
	if err := db.Model(&model.XrayTrafficReceipt{}).Where("id = ?", id).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("idempotency receipt was not retained: %d %v", count, err)
	}
	if err := db.Exec("DROP TRIGGER reject_poll_marker_delete").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.applyPending(j.applyJournaledXrayBatch); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&counter, counter.Id).Error; err != nil || counter.Up != 123 {
		t.Fatalf("retry duplicated or lost traffic: %+v %v", counter, err)
	}
	if err := db.Model(&model.XrayTrafficInFlight{}).Where("id = ?", id).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("poll marker remained after retry: %d %v", count, err)
	}
}

func TestLegacyJournalWithoutPollMarkerBlocksAllocationUntilReplay(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	r := model.Reseller{Username: "legacy-journal", Password: "hash", Enable: true, TrafficLimit: 1000}
	client := model.ClientRecord{Email: "legacy-journal@example.test", Enable: true, TotalGB: 100}
	counter := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, v := range []any{&r, &client, &counter} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.ResellerClient{ResellerId: r.Id, Email: client.Email}).Error; err != nil {
		t.Fatal(err)
	}
	batch := &xrayTrafficBatch{id: uuid.NewString(), clients: []*xray.ClientTraffic{{Email: client.Email, Up: 123}}}
	var err error
	batch.identities, err = (&service.InboundService{}).CaptureXrayTrafficIdentities(batch.clients)
	if err != nil {
		t.Fatal(err)
	}
	if err := storeXrayBatch(batch); err != nil {
		t.Fatal(err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("legacy journal did not block allocation")
	}
	if err := (&service.ResellerService{}).CheckTrafficQuota(&r, 1); err == nil {
		t.Fatal("legacy journal did not block direct quota check")
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&counter, counter.Id).Error; err != nil || counter.Up != 123 {
		t.Fatalf("legacy journal not applied: %+v %v", counter, err)
	}
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatalf("recovered journal still blocked allocation: %v", err)
	}
	unlock()
}

func TestUnexpectedXrayJournalFilesBlockRecoveryAndAllocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	if err := os.MkdirAll(xrayJournalDir(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(xrayJournalDir(), ".pending-incomplete")
	if err := os.WriteFile(path, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("incomplete journal did not block allocation")
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err == nil {
		t.Fatal("unexpected journal file silently ignored")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unexpected journal evidence removed: %v", err)
	}
}
