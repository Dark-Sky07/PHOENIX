package job

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// Uses only the isolated PostgreSQL CI database; it must not run in parallel
// with tests that replace the process-global database connection.
func TestTuicCommittedPollRecoveryPostgres(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	t.Setenv("XUI_DB_TYPE", "postgres")
	t.Setenv("XUI_DB_DSN", dsn)
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := database.InitDB(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	id := uuid.NewString()
	row := xray.ClientTraffic{Email: id + "@tuic-recovery.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sharedTuicPoll.unlock != nil {
			sharedTuicPoll.unlock()
			sharedTuicPoll = tuicPollState{}
		}
		_ = db.Where("id = ?", id).Delete(&model.XrayTrafficInFlight{}).Error
		_ = db.Where("id = ?", id).Delete(&model.TuicTrafficReceipt{}).Error
		_ = db.Delete(&row).Error
	})
	batch := tuicTrafficBatch{ID: id, PollMarker: true, Deltas: []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 42}}}
	if err := service.BeginXrayPoll(id); err != nil {
		t.Fatal(err)
	}
	if _, err := storeTuicBatch(batch); err != nil {
		t.Fatal(err)
	}
	if err := (&service.InboundService{}).AddTuicTrafficBatch(id, batch.Deltas); err != nil {
		t.Fatal(err)
	}
	if err := NewTuicJob().replayTuicJournal(); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 42 {
		t.Fatalf("recovery changed committed usage: %+v %v", row, err)
	}
	if err := service.VerifyXrayPollMarker(id); err == nil {
		t.Fatal("committed poll marker remains")
	}
	// PostgreSQL's trim does not remove tabs; this colliding row must still
	// block a fresh batch and leave neither a receipt nor changed counters.
	canonical := []model.Client{{Email: row.Email, Enable: true}}
	if filtered, err := (&service.InboundService{}).FilterDaemonAccountingClients(canonical); err != nil || len(filtered) != 1 {
		t.Fatalf("valid daemon identity rejected: %+v %v", filtered, err)
	}
	other := xray.ClientTraffic{Email: "\t" + row.Email + "\n", Enable: true}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Delete(&other).Error })
	if filtered, err := (&service.InboundService{}).FilterDaemonAccountingClients(canonical); err != nil || len(filtered) != 0 || filtered == nil {
		t.Fatalf("ambiguous daemon identity accepted or filter returned nil: %+v %v", filtered, err)
	}
	badID := uuid.NewString()
	t.Cleanup(func() { _ = db.Where("id = ?", badID).Delete(&model.TuicTrafficReceipt{}).Error })
	if err := (&service.InboundService{}).AddTuicTrafficBatch(badID, batch.Deltas); err == nil {
		t.Fatal("Go-normalized ambiguous destination accepted")
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 42 {
		t.Fatalf("rejected batch changed usage: %+v %v", row, err)
	}
	var count int64
	if err := db.Model(&model.TuicTrafficReceipt{}).Where("id = ?", badID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rejected batch left receipt: %d %v", count, err)
	}
}
