package service

import (
	"os"
	"path/filepath"
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

func TestCounterIncidentStatusStorageErrorsAreUnavailable(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	// A file in place of the fallback directory makes inspection fail even when
	// the database is healthy. Do not report a clear summary in this case.
	if err := os.WriteFile(counterIncidentDir(), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readCounterIncidentStatus(db); got.Available {
		t.Fatalf("failed fallback inspection reported available: %+v", got)
	}
	if err := os.Remove(counterIncidentDir()); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&model.ResellerNodeCounterIncident{}); err != nil {
		t.Fatal(err)
	}
	if got := readCounterIncidentStatus(db); got.Available {
		t.Fatalf("failed database query reported available: %+v", got)
	}
}

func TestCounterIncidentStatusIncludesXrayPollMarkers(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	if err := db.Create(&model.XrayTrafficInFlight{ID: "interrupted-poll"}).Error; err != nil {
		t.Fatal(err)
	}
	got := readCounterIncidentStatus(db)
	if !got.Available || got.DatabaseCount != 1 {
		t.Fatalf("poll marker not visible: %+v", got)
	}
	if err := db.Migrator().DropTable(&model.XrayTrafficInFlight{}); err != nil {
		t.Fatal(err)
	}
	if got := readCounterIncidentStatus(db); got.Available {
		t.Fatalf("poll-marker query failure reported available: %+v", got)
	}
}

func TestCounterIncidentStatusShowsOrCannotInspectXrayJournal(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	if err := os.MkdirAll(XrayTrafficJournalDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(XrayTrafficJournalDir(), ".pending-incomplete"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readCounterIncidentStatus(db); !got.Available || !got.FallbackPresent {
		t.Fatalf("incomplete local journal hidden: %+v", got)
	}
	if err := os.RemoveAll(XrayTrafficJournalDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(XrayTrafficJournalDir(), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readCounterIncidentStatus(db); got.Available {
		t.Fatalf("failed journal inspection reported available: %+v", got)
	}
}
