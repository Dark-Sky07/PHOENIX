package trafficprotocol

import (
	"bytes"
	"errors"
	"math"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type appliedSnapshot struct {
	ID   int `gorm:"primaryKey"`
	Data string
}

func stateDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "state.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.TrafficSnapshotSender{}, &model.TrafficSnapshotReceiver{}, &appliedSnapshot{}); err != nil {
		t.Fatal(err)
	}
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return db
}
func TestDurableSequenceAndAtomicVerifiedApply(t *testing.T) {
	db := stateDB(t)
	for _, row := range []any{&model.TrafficSnapshotSender{ID: "peer", Sender: "master", Generation: "gen"}, &model.TrafficSnapshotReceiver{ID: "peer", Sender: "master", Generation: "gen"}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	e, err := ReserveSequence(db, "peer")
	if err != nil || e.Sequence != 1 {
		t.Fatalf("reservation: %+v %v", e, err)
	}
	key := bytes.Repeat([]byte{0x43}, 32)
	e.Snapshot = []byte("one")
	e, err = Sign(e, key)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected failure")
	if err := ApplyVerified(db, "peer", key, e, func(tx *gorm.DB, b []byte) error {
		if err := tx.Create(&appliedSnapshot{Data: string(b)}).Error; err != nil {
			return err
		}
		return failure
	}); !errors.Is(err, failure) {
		t.Fatalf("rollback: %v", err)
	}
	var count int64
	if err := db.Model(&appliedSnapshot{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rollback snapshot: %d %v", count, err)
	}
	apply := func(tx *gorm.DB, b []byte) error { return tx.Create(&appliedSnapshot{Data: string(b)}).Error }
	if err := ApplyVerified(db, "peer", key, e, apply); err != nil {
		t.Fatal(err)
	}
	if err := ApplyVerified(db, "peer", key, e, apply); err == nil {
		t.Fatal("replay accepted")
	}
	if err := db.Model(&appliedSnapshot{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("replay applied: %d %v", count, err)
	}
	e2, err := ReserveSequence(db, "peer")
	if err != nil || e2.Sequence != 2 {
		t.Fatalf("next: %+v %v", e2, err)
	}
	e2.Snapshot = []byte("two")
	e2, err = Sign(e2, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyVerified(db, "peer", bytes.Repeat([]byte{0x44}, 32), e2, apply); err == nil {
		t.Fatal("wrong key accepted")
	}
	if err := ApplyVerified(db, "peer", key, e2, apply); err != nil {
		t.Fatal(err)
	}
	var state model.TrafficSnapshotReceiver
	if err := db.First(&state, "id = ?", "peer").Error; err != nil || state.LastSequence != 2 {
		t.Fatalf("sequence: %+v %v", state, err)
	}
}
func TestSequenceStateRejectsUnknownAndExhausted(t *testing.T) {
	db := stateDB(t)
	if _, err := ReserveSequence(db, "missing"); err == nil {
		t.Fatal("missing sender accepted")
	}
	if err := db.Create(&model.TrafficSnapshotSender{ID: "full", Sender: "s", Generation: "g", LastSequence: math.MaxInt64}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := ReserveSequence(db, "full"); err == nil {
		t.Fatal("exhausted sender accepted")
	}
	key := bytes.Repeat([]byte{1}, 32)
	e, err := Sign(Envelope{Version: Version, Sender: "s", Generation: "g", Sequence: 1, Snapshot: []byte("x")}, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyVerified(db, "missing", key, e, func(*gorm.DB, []byte) error { return nil }); err == nil {
		t.Fatal("missing receiver accepted")
	}
}

// Competing workers may receive SQLite busy errors, but must never both apply.
func TestConcurrentSnapshotReplayAtMostOnce(t *testing.T) {
	db := stateDB(t)
	if err := db.Create(&model.TrafficSnapshotReceiver{ID: "peer", Sender: "s", Generation: "g"}).Error; err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{3}, 32)
	e, err := Sign(Envelope{Version: Version, Sender: "s", Generation: "g", Sequence: 1, Snapshot: []byte("x")}, key)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 8
	start := make(chan struct{})
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- ApplyVerified(db, "peer", key, e, func(tx *gorm.DB, b []byte) error { return tx.Create(&appliedSnapshot{Data: string(b)}).Error })
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("expected exactly one commit, got %d", success)
	}
	var count int64
	if err := db.Model(&appliedSnapshot{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("applied rows: %d, %v", count, err)
	}
}

func TestReceiverSenderCannotHaveIndependentSequenceStreams(t *testing.T) {
	db := stateDB(t)
	if err := db.Create(&model.TrafficSnapshotReceiver{ID: "token-one", Sender: "master", Generation: "g"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.TrafficSnapshotReceiver{ID: "token-two", Sender: "master", Generation: "other"}).Error; err == nil {
		t.Fatal("one sender was registered with two independent receiver sequences")
	}
}
