package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestXrayTrafficBatchRetryIsAtomicAndIdempotent(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	client := model.ClientRecord{Email: "batch@example.test", Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	row := xray.ClientTraffic{Email: client.Email, Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_xray_update BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected traffic failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	svc := &InboundService{}
	delta := []*xray.ClientTraffic{{Email: client.Email, Up: 123, Down: 456}}
	if _, _, err := svc.AddXrayTrafficBatch("batch-1", nil, delta); err == nil {
		t.Fatal("injected failure should abort batch")
	}
	var count int64
	if err := db.Model(&model.XrayTrafficReceipt{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("receipt after rollback: %d, %v", count, err)
	}
	if err := db.Exec("DROP TRIGGER reject_xray_update").Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := svc.AddXrayTrafficBatch("batch-1", nil, delta); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 123 || row.Down != 456 {
		t.Fatalf("batch replay double counted or lost traffic: %d/%d", row.Up, row.Down)
	}
	if err := svc.ForgetXrayTrafficBatch("batch-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.XrayTrafficReceipt{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("receipt cleanup: %d, %v", count, err)
	}
}
