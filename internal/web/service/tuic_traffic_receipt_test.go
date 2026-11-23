package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestTuicReceiptBindsOriginalDeltas(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "tuic-receipt@example.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	svc := &InboundService{}
	batch := []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 12, UUID: "original"}}
	for i := 0; i < 2; i++ {
		if err := svc.AddTuicTrafficBatch("batch", batch); err != nil {
			t.Fatal(err)
		}
	}
	conflict := append([]tuic.ClientTrafficDelta(nil), batch...)
	conflict[0].UUID = "different"
	if err := svc.AddTuicTrafficBatch("batch", conflict); err == nil {
		t.Fatal("same ID with different raw payload accepted")
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 12 {
		t.Fatalf("conflicting replay changed usage: %+v %v", row, err)
	}
	var receipt model.TuicTrafficReceipt
	if err := db.First(&receipt, "id = ?", "batch").Error; err != nil || len(receipt.PayloadHash) != 67 {
		t.Fatalf("receipt hash missing: %+v %v", receipt, err)
	}
	if err := db.Create(&model.TuicTrafficReceipt{ID: "legacy"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AddTuicTrafficBatch("legacy", batch); err == nil {
		t.Fatal("legacy receipt accepted without payload evidence")
	}
}

func TestTuicBatchRejectsNegativeOnlyCounter(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "negative-tuic@example.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	sample := []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: -1, Down: -2}}
	if err := (&InboundService{}).AddTuicTrafficBatch("negative-only", sample); err == nil {
		t.Fatal("negative-only sample accepted")
	}
	var count int64
	if err := db.Model(&model.TuicTrafficReceipt{}).Where("id = ?", "negative-only").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("negative sample created receipt: %d %v", count, err)
	}
}

func TestTuicJournalRejectsMissingAndAmbiguousDestinations(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	first := xray.ClientTraffic{Email: "alice@example.test", Enable: true}
	if err := db.Create(&first).Error; err != nil {
		t.Fatal(err)
	}
	other := xray.ClientTraffic{Email: "ALICE@example.test", Enable: true}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	service := &InboundService{}
	for _, tc := range []struct {
		id     string
		sample tuic.ClientTrafficDelta
	}{
		{"ambiguous", tuic.ClientTrafficDelta{Email: first.Email, TrafficID: first.Id, Up: 1}},
		{"no-id", tuic.ClientTrafficDelta{Email: first.Email, Up: 1}},
		{"absent-id", tuic.ClientTrafficDelta{Email: first.Email, TrafficID: other.Id + 10000, Up: 1}},
	} {
		if err := service.AddTuicTrafficBatch(tc.id, []tuic.ClientTrafficDelta{tc.sample}); err == nil {
			t.Fatalf("%s destination accepted", tc.id)
		}
		var receipt model.TuicTrafficReceipt
		if err := db.Where("id = ?", tc.id).Take(&receipt).Error; err == nil {
			t.Fatalf("%s receipt persisted", tc.id)
		}
	}
	if err := db.Delete(&other).Error; err != nil {
		t.Fatal(err)
	}
	// A known traffic row can be renamed without redirecting to another row.
	if err := service.AddTuicTrafficBatch("renamed", []tuic.ClientTrafficDelta{{Email: "old@example.test", TrafficID: first.Id, Up: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&first, first.Id).Error; err != nil || first.Up != 3 {
		t.Fatalf("renamed identity usage incorrect: %+v %v", first, err)
	}
}
