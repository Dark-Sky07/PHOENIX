package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

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
		sample *xray.ClientTraffic
	}{
		{"ambiguous", &xray.ClientTraffic{Email: first.Email, TuicTrafficID: first.Id, Up: 1}},
		{"no-id", &xray.ClientTraffic{Email: first.Email, Up: 1}},
		{"absent-id", &xray.ClientTraffic{Email: first.Email, TuicTrafficID: other.Id + 10000, Up: 1}},
	} {
		if err := service.AddTuicTrafficBatch(tc.id, []*xray.ClientTraffic{tc.sample}); err == nil {
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
	if err := service.AddTuicTrafficBatch("renamed", []*xray.ClientTraffic{{Email: "old@example.test", TuicTrafficID: first.Id, Up: 3}}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&first, first.Id).Error; err != nil || first.Up != 3 {
		t.Fatalf("renamed identity usage incorrect: %+v %v", first, err)
	}
}
