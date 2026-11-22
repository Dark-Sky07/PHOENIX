package service

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("deletion of %T: expected PostgreSQL lock timeout, got %v", table, err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("email = ?", email).Delete(&model.ResellerClient{}).Error; err != nil {
		t.Fatal(err)
	}
}

// Independent database sessions must contend on the same advisory lock.
func TestResellerAllocationAdvisoryLockPostgres(t *testing.T) {
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
	unlock, ok, err := acquirePostgresAllocationLock(db, true)
	if err != nil || !ok {
		t.Fatalf("initial acquisition: %v %v", ok, err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	second, ok, err := acquirePostgresAllocationLock(db, false)
	if err != nil || ok || second != nil {
		t.Fatalf("competing acquisition: %v %v", ok, err)
	}
	unlock()
	unlock = nil
	second, ok, err = acquirePostgresAllocationLock(db, false)
	if err != nil || !ok {
		t.Fatalf("acquisition after release: %v %v", ok, err)
	}
	second()
}

func TestResellerNodeBaselinePostgres(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.AutoMigrate(&model.ClientRecord{}, &model.ResellerClient{}, &model.Inbound{}, &model.ClientInbound{}, &model.NodeClientTraffic{}, &model.ResellerNodeCounterIncident{}); err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	const nodeID = 90001
	client := model.ClientRecord{Email: uuid.NewString() + "@node-baseline.test", TotalGB: 100, Enable: true}
	inbound := model.Inbound{Tag: uuid.NewString(), NodeID: func() *int { n := nodeID; return &n }(), Protocol: model.VLESS}
	for _, row := range []any{&client, &inbound} {
		if err := tx.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []any{&model.ResellerClient{ResellerId: 90001, Email: client.Email}, &model.ClientInbound{ClientId: client.Id, InboundId: inbound.Id}} {
		if err := tx.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := validateResellerNodeBaselines(tx, 90001); err == nil {
		t.Fatal("missing baseline accepted")
	}
	if err := tx.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: client.Email, Up: 5, Down: 6}).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateResellerNodeBaselines(tx, 90001); err != nil {
		t.Fatal(err)
	}
	incident := model.ResellerNodeCounterIncident{NodeId: nodeID, Email: client.Email, PreviousUp: 5, PreviousDown: 6, ObservedUp: 2, ObservedDown: 6}
	if err := tx.Create(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateResellerNodeBaselines(tx, 90001); err == nil {
		t.Fatal("incident did not block reseller")
	}
	if err := tx.Where("client_id = ?", client.Id).Delete(&model.ClientInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := validateResellerNodeBaselines(tx, 90001); err == nil {
		t.Fatal("detachment hid incident")
	}
	if err := validateResellerNodeBaselines(tx, 90002); err != nil {
		t.Fatalf("incident affected another reseller: %v", err)
	}
}

// PostgreSQL coverage for remote evidence that survives a missing association.
func TestResellerRemoteGuardOrphanedEvidencePostgres(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.AutoMigrate(&model.ClientRecord{}, &model.ResellerClient{}, &model.ResellerInbound{}, &model.Inbound{}, &model.ClientInbound{}, &model.NodeClientTraffic{}); err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	const reseller = 987659
	client := model.ClientRecord{Email: uuid.NewString() + "@guard.test"}
	if err := tx.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Create(&model.ResellerClient{ResellerId: reseller, Email: client.Email}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectResellerUnverifiedNodes(tx, reseller); err != nil {
		t.Fatal(err)
	}
	if err := tx.Create(&model.ResellerInbound{ResellerId: reseller, InboundId: 999999991}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectResellerUnverifiedNodes(tx, reseller); err == nil {
		t.Fatal("orphaned reseller inbound accepted")
	}
	if err := tx.Where("reseller_id = ?", reseller).Delete(&model.ResellerInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Create(&model.ClientInbound{ClientId: client.Id, InboundId: 999999992}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectResellerUnverifiedNodes(tx, reseller); err == nil {
		t.Fatal("orphaned client link accepted")
	}
	if err := tx.Where("client_id = ?", client.Id).Delete(&model.ClientInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Create(&model.NodeClientTraffic{NodeId: 999999, Email: "  " + strings.ToUpper(client.Email) + "  ", Up: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectResellerUnverifiedNodes(tx, reseller); err == nil {
		t.Fatal("orphaned baseline accepted")
	}
}
