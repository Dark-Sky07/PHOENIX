package trafficprotocol

import (
	"bytes"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

type postgresSnapshotProbe struct {
	ID    string `gorm:"primaryKey"`
	Value int64
}

// Uses a disposable test database supplied by CI. Workers use independent
// database connections and contend for the same sender/receiver state.
func TestSnapshotSequencePostgresConcurrency(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(20)
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.TrafficSnapshotSender{}, &model.TrafficSnapshotReceiver{}, &postgresSnapshotProbe{}); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	for _, v := range []any{&model.TrafficSnapshotSender{ID: id, Sender: "sender", Generation: "generation"}, &model.TrafficSnapshotReceiver{ID: id, Sender: "sender", Generation: "generation"}, &postgresSnapshotProbe{ID: id}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		db.Where("id = ?", id).Delete(&model.TrafficSnapshotSender{})
		db.Where("id = ?", id).Delete(&model.TrafficSnapshotReceiver{})
		db.Where("id = ?", id).Delete(&postgresSnapshotProbe{})
	})
	const workers = 12
	start := make(chan struct{})
	reserved := make(chan Envelope, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			e, err := ReserveSequence(db, id)
			if err == nil {
				reserved <- e
			}
		}()
	}
	close(start)
	wg.Wait()
	close(reserved)
	seen := map[uint64]bool{}
	for e := range reserved {
		if seen[e.Sequence] {
			t.Fatalf("duplicate reserved sequence %d", e.Sequence)
		}
		seen[e.Sequence] = true
	}
	if len(seen) == 0 {
		t.Fatal("no sender reservation committed")
	}
	var sender model.TrafficSnapshotSender
	if err := db.First(&sender, "id = ?", id).Error; err != nil || sender.LastSequence != int64(len(seen)) {
		t.Fatalf("reservation state: %+v, %v, successful %d", sender, err, len(seen))
	}
	key := bytes.Repeat([]byte{7}, 32)
	envelope, err := Sign(Envelope{Version: Version, Sender: "sender", Generation: "generation", Sequence: 1, Snapshot: []byte("snapshot")}, key)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected")
	update := func(tx *gorm.DB, _ []byte) error {
		return tx.Model(&postgresSnapshotProbe{}).Where("id = ?", id).UpdateColumn("value", gorm.Expr("value + 1")).Error
	}
	if err := ApplyVerified(db, id, key, envelope, func(tx *gorm.DB, b []byte) error {
		if err := update(tx, b); err != nil {
			return err
		}
		return injected
	}); !errors.Is(err, injected) {
		t.Fatalf("expected rollback: %v", err)
	}
	start = make(chan struct{})
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- ApplyVerified(db, id, key, envelope, update) }()
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
	var probe postgresSnapshotProbe
	var receiver model.TrafficSnapshotReceiver
	if err := db.First(&probe, "id = ?", id).Error; err != nil || probe.Value != 1 {
		t.Fatalf("applied effects: %+v, %v", probe, err)
	}
	if err := db.First(&receiver, "id = ?", id).Error; err != nil || receiver.LastSequence != 1 {
		t.Fatalf("receiver sequence: %+v, %v", receiver, err)
	}
}
