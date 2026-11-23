package job

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestTuicJournalRejectsConflictingPublicationAndReplay(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "receipt-tuic@example.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	batch := tuicTrafficBatch{ID: "receipt-identity", Deltas: []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 10}}}
	path, err := storeTuicBatch(batch)
	if err != nil {
		t.Fatal(err)
	}
	conflict := tuicTrafficBatch{ID: batch.ID, Deltas: []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 99}}}
	if _, err := storeTuicBatch(conflict); err == nil {
		t.Fatal("conflicting publication overwrote original journal")
	}
	job := NewTuicJob()
	if err := job.replayTuicJournal(); err != nil {
		t.Fatal(err)
	}
	if _, err := storeTuicBatch(conflict); err != nil {
		t.Fatal(err)
	}
	if err := job.replayTuicJournal(); err == nil {
		t.Fatal("conflicting journal accepted against receipt")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("conflicting journal removed: %v", err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 10 {
		t.Fatalf("conflicting replay changed usage: %+v %v", row, err)
	}
}

func TestTuicJournalRejectsUnsafeIDsAndSymlinks(t *testing.T) {
	setupIntegrationDB(t)
	if _, err := storeTuicBatch(tuicTrafficBatch{ID: "../outside"}); err == nil {
		t.Fatal("unsafe journal ID accepted")
	}
	if err := os.MkdirAll(tuicJournalDir(), 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tuicJournalDir(), "symlink.json")
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := storeTuicBatch(tuicTrafficBatch{ID: "symlink"}); err == nil {
		t.Fatal("symlink journal accepted")
	}
	if err := NewTuicJob().replayTuicJournal(); err == nil {
		t.Fatal("symlink journal replayed")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "{}" {
		t.Fatalf("symlink target changed: %q %v", got, err)
	}
}

func TestTuicJournalMissingDestinationRetainsEvidence(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	old := xray.ClientTraffic{Email: "reused-tuic@example.test", Enable: true}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&old).Error; err != nil {
		t.Fatal(err)
	}
	replacement := xray.ClientTraffic{Email: old.Email, Enable: true}
	if err := db.Create(&replacement).Error; err != nil {
		t.Fatal(err)
	}
	batch := tuicTrafficBatch{ID: "missing-identity", Deltas: []tuic.ClientTrafficDelta{{Email: old.Email, TrafficID: old.Id, Up: 123}}}
	path, err := storeTuicBatch(batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewTuicJob().replayTuicJournal(); err == nil {
		t.Fatal("missing TUIC traffic ID was silently acknowledged")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal evidence removed: %v", err)
	}
	if err := db.First(&replacement, replacement.Id).Error; err != nil || replacement.Up != 0 {
		t.Fatalf("replacement charged: %+v %v", replacement, err)
	}
	var receipts int64
	if err := db.Table("tuic_traffic_receipts").Where("id = ?", batch.ID).Count(&receipts).Error; err != nil || receipts != 0 {
		t.Fatalf("unaccounted receipt persisted: %d %v", receipts, err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("local unresolved TUIC journal did not block allocation")
	}
}

func TestTuicJournalUnknownEntryBlocksReplay(t *testing.T) {
	setupIntegrationDB(t)
	if err := os.MkdirAll(tuicJournalDir(), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tuicJournalDir(), ".pending-incomplete")
	if err := os.WriteFile(path, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewTuicJob().replayTuicJournal(); err == nil {
		t.Fatal("incomplete TUIC journal ignored")
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("incomplete TUIC journal allowed allocation")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("partial evidence lost: %v", err)
	}
}

func TestTuicAggregationSaturates(t *testing.T) {
	got := aggregateTuicClientTraffic([]tuic.ClientTrafficDelta{{TrafficID: 1, Up: math.MaxInt64 - 1}, {TrafficID: 1, Up: 42}}, nil)
	if len(got) != 1 || got[0].Up != math.MaxInt64 {
		t.Fatalf("TUIC aggregation wrapped: %+v", got)
	}
}

func TestTuicShutdownJournalSurvivesDatabaseFailureAndReplaysOnce(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "journal@x", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	batch := tuicTrafficBatch{ID: "shutdown-batch", Deltas: []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 123, Down: 456}}}
	path, err := storeTuicBatch(batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_tuic_write BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(FAIL, 'disk fault'); END`).Error; err != nil {
		t.Fatal(err)
	}
	job := NewTuicJob()
	if err := job.FlushStoppedTraffic(); err == nil {
		t.Fatal("failed traffic update must return an error")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("durable recovery file missing: %v", err)
	}
	var receipts int64
	if err := db.Table("tuic_traffic_receipts").Count(&receipts).Error; err == nil && receipts != 0 {
		t.Fatal("failed transaction committed a receipt")
	}
	if err := db.Exec("DROP TRIGGER fail_tuic_write").Error; err != nil {
		t.Fatal(err)
	}
	// A new job has no knowledge of the previous process's in-memory deltas.
	recovered := NewTuicJob()
	if err := recovered.replayTuicJournal(); err != nil {
		t.Fatal(err)
	}
	// Recreate a stale file, as if file removal was lost after DB commit.
	if _, err := storeTuicBatch(batch); err != nil {
		t.Fatal(err)
	}
	if err := recovered.replayTuicJournal(); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("id = ?", row.Id).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 123 || row.Down != 456 {
		t.Fatalf("replay billed %d/%d", row.Up, row.Down)
	}
	entries, err := os.ReadDir(tuicJournalDir())
	if err != nil || len(entries) != 0 {
		t.Fatalf("journal not drained: %v %v", entries, err)
	}
}
