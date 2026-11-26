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
	if err := db.Model(&model.XrayTrafficReceipt{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("receipt not retained for future replay: %d, %v", count, err)
	}
	var receipt model.XrayTrafficReceipt
	if err := db.First(&receipt, "id = ?", "batch-1").Error; err != nil || len(receipt.PayloadHash) != 67 {
		t.Fatalf("payload hash not persisted: %+v %v", receipt, err)
	}
	changed := []*xray.ClientTraffic{{Email: client.Email, Up: 124, Down: 456}}
	if _, _, err := svc.AddXrayTrafficBatch("batch-1", nil, changed); err == nil {
		t.Fatal("same batch ID with different payload accepted")
	}
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil || row.Up != 123 || row.Down != 456 {
		t.Fatalf("conflicting replay changed usage: %+v %v", row, err)
	}
}

func TestLegacyXrayReceiptRequiresManualReview(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	if err := db.Create(&model.XrayTrafficReceipt{ID: "legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.XrayTrafficInFlight{ID: "legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&InboundService{}).AddCheckedXrayTrafficBatch("legacy", nil, nil, []XrayTrafficIdentity{}); err == nil {
		t.Fatal("legacy receipt allowed unverified replay")
	}
	if err := RecoverCommittedXrayPollMarkers(); err != nil {
		t.Fatal(err)
	}
	if err := VerifyXrayPollMarker("legacy"); err != nil {
		t.Fatalf("legacy marker cleared without payload evidence: %v", err)
	}
}

func TestXrayReceiptRejectsUnserializedProtocolFields(t *testing.T) {
	setupBulkDB(t)
	for _, sample := range []*xray.ClientTraffic{
		{Email: "user", PPPSessionID: "session"},
		{Email: "user", PPPInboundID: 1},
		{Email: "user", PPPRxBytes: 1},
		{Email: "user", TuicTrafficID: 1},
		{Email: "user", TuicUUID: "uuid"},
		{Email: "user", TuicInboundId: 1},
	} {
		if _, _, err := (&InboundService{}).AddXrayTrafficBatch("bad-payload", nil, []*xray.ClientTraffic{sample}); err == nil {
			t.Fatalf("unserialized fields accepted: %+v", sample)
		}
	}
	var count int64
	if err := database.GetDB().Model(&model.XrayTrafficReceipt{}).Where("id = ?", "bad-payload").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("receipt created for invalid payload: %d %v", count, err)
	}
}
