package service

import (
	"os"
	"sync"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Two independent DB transactions reproduce separate panel writers; the
// process-local traffic-writer mutex is deliberately not involved.
func TestPPPCheckpointConcurrentWritersPostgres(t *testing.T) {
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
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.PPPClientTrafficCounter{}, &xray.ClientTraffic{}); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	email := key + "@ppp-concurrency.test"
	row := xray.ClientTraffic{Email: email, Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Where("session_id = ?", key).Delete(&model.PPPClientTrafficCounter{}).Error
		_ = db.Delete(&row).Error
	})
	run := func(rx, txBytes int64) {
		t.Helper()
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- db.Transaction(func(tx *gorm.DB) error {
					rows, err := resolvePPPClientTraffic(tx, []*xray.ClientTraffic{{Email: email, PPPSessionID: key, PPPInboundID: 321, PPPRxBytes: rx, PPPTxBytes: txBytes}})
					if err != nil {
						return err
					}
					return tx.Model(&xray.ClientTraffic{}).Where("id = ?", row.Id).Updates(map[string]any{"up": gorm.Expr("up + ?", rows[0].Up), "down": gorm.Expr("down + ?", rows[0].Down)}).Error
				})
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent writer failed: %v", err)
			}
		}
		var got xray.ClientTraffic
		if err := db.First(&got, row.Id).Error; err != nil || got.Up != rx || got.Down != txBytes {
			t.Fatalf("double charged: %+v %v", got, err)
		}
	}
	run(120, 150) // both writers concurrently create the same checkpoint
	run(220, 250) // both writers concurrently update an existing checkpoint
}
