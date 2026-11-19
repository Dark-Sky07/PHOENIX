package service

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Separate database transactions verify row locking, not just goroutine locks.
// Run only against an isolated test database.
func TestXrayTrafficIdentityLocksExistingRowsPostgres(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, table := range []any{&model.ClientRecord{}, &xray.ClientTraffic{}, &model.ResellerClient{}} {
		if err := db.AutoMigrate(table); err != nil {
			t.Fatal(err)
		}
	}
	email := uuid.NewString() + "@lock.test"
	client := model.ClientRecord{Email: email, Enable: true}
	counter := xray.ClientTraffic{Email: email, Enable: true}
	owner := model.ResellerClient{Email: email, ResellerId: 123}
	for _, row := range []any{&client, &counter, &owner} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = db.Where("email = ?", email).Delete(&model.ResellerClient{}).Error
		_ = db.Where("email = ?", email).Delete(&xray.ClientTraffic{}).Error
		_ = db.Where("email = ?", email).Delete(&model.ClientRecord{}).Error
	})
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if _, err := captureXrayTrafficIdentities(tx, []*xray.ClientTraffic{{Email: email, Up: 1}}, true); err != nil {
		t.Fatal(err)
	}
	for _, table := range []any{&model.ClientRecord{}, &xray.ClientTraffic{}, &model.ResellerClient{}} {
		other := db.Begin()
		if other.Error != nil {
			t.Fatal(other.Error)
		}
		if err := other.Exec("SET LOCAL lock_timeout = '150ms'").Error; err != nil {
			other.Rollback()
			t.Fatal(err)
		}
		err := other.Where("email = ?", email).Delete(table).Error
		other.Rollback()
		if err == nil {
			t.Fatalf("concurrent deletion of %T was not blocked", table)
		}
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("email = ?", email).Delete(&model.ResellerClient{}).Error; err != nil {
		t.Fatal(err)
	}
}
