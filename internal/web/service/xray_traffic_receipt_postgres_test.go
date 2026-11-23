package service

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// Run only on the isolated PostgreSQL CI database.
func TestXrayReceiptPayloadBindingPostgres(t *testing.T) {
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
	email := id + "@receipt.test"
	row := xray.ClientTraffic{Email: email, Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Where("id = ?", id).Delete(&model.XrayTrafficReceipt{}).Error
		_ = db.Delete(&row).Error
	})
	svc := &InboundService{}
	batch := []*xray.ClientTraffic{{Email: email, Up: 100}}
	for i := 0; i < 2; i++ {
		if _, _, err := svc.AddXrayTrafficBatch(id, nil, batch); err != nil {
			t.Fatalf("identical retry %d: %v", i, err)
		}
	}
	if _, _, err := svc.AddXrayTrafficBatch(id, nil, []*xray.ClientTraffic{{Email: email, Up: 101}}); err == nil {
		t.Fatal("different payload accepted for committed ID")
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 100 {
		t.Fatalf("incorrect usage: %+v %v", row, err)
	}
	var receipt model.XrayTrafficReceipt
	if err := db.First(&receipt, "id = ?", id).Error; err != nil || len(receipt.PayloadHash) != 67 {
		t.Fatalf("missing receipt hash: %+v %v", receipt, err)
	}
}
