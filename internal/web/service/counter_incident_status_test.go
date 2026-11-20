package service

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestCounterIncidentStatusDoesNotTreatUnavailableAsClear(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	got := readCounterIncidentStatus(db)
	if !got.Available || got.DatabaseCount != 0 || got.FallbackPresent || got.PendingInProcess {
		t.Fatalf("empty status: %+v", got)
	}
	incident := model.ResellerNodeCounterIncident{NodeId: 77, Email: client.Email, PreviousUp: 100, ObservedUp: 2}
	if err := db.Create(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if err := storeCounterIncidentFallback(incident); err != nil {
		t.Fatal(err)
	}
	got = readCounterIncidentStatus(db)
	if !got.Available || got.DatabaseCount != 1 || !got.FallbackPresent {
		t.Fatalf("incident status: %+v", got)
	}
	if err := os.RemoveAll(counterIncidentDir()); err != nil {
		t.Fatal(err)
	}
	got = readCounterIncidentStatus(nil)
	if got.Available {
		t.Fatal("missing database was reported as clear")
	}
	xrayCoreExitPending.Store(true)
	t.Cleanup(func() { xrayCoreExitPending.Store(false) })
	got = readCounterIncidentStatus(db)
	if !got.PendingInProcess {
		t.Fatal("pending in-memory observation was hidden")
	}
}
