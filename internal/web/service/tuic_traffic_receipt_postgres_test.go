package service

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// Run only on the isolated PostgreSQL CI database.
func TestTuicReceiptPayloadBindingPostgres(t *testing.T) {
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
	row := xray.ClientTraffic{Email: id + "@tuic-receipt.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Where("id = ?", id).Delete(&model.TuicTrafficReceipt{}).Error
		_ = db.Delete(&row).Error
	})
	batch := []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 100}}
	svc := &InboundService{}
	for i := 0; i < 2; i++ {
		if err := svc.AddTuicTrafficBatch(id, batch); err != nil {
			t.Fatalf("identical retry %d: %v", i, err)
		}
	}
	conflict := append([]tuic.ClientTrafficDelta(nil), batch...)
	conflict[0].Up++
	if err := svc.AddTuicTrafficBatch(id, conflict); err == nil {
		t.Fatal("conflicting TUIC payload accepted")
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 100 {
		t.Fatalf("incorrect usage: %+v %v", row, err)
	}
}
