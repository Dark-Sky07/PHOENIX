package service

import (
	"math"
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/google/uuid"
)

func TestVoucherTrafficOverflowDoesNotGrantUnlimitedQuota(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "voucher-overflow", Password: "hash", Enable: true, TrafficLimit: math.MaxInt64 - 1}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	v := model.ResellerVoucher{Code: "OVERFLOW", TrafficGB: 1, MaxUses: 1}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&VoucherService{}).Redeem(r.Id, v.Code); err == nil {
		t.Fatal("overflowing redemption succeeded")
	}
	if err := db.First(&r, r.Id).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&v, v.Id).Error; err != nil {
		t.Fatal(err)
	}
	if r.TrafficLimit != math.MaxInt64-1 || v.UsedCount != 0 {
		t.Fatalf("failed redemption mutated account or voucher: %+v %+v", r, v)
	}
	if _, err := (&VoucherService{}).Create("TOO-LARGE", 0, math.MaxInt64, 1, 0, ""); err == nil {
		t.Fatal("oversized voucher accepted")
	}
}

func TestConcurrentVoucherRedemptionPostgres(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	t.Setenv("XUI_DB_TYPE", "postgres")
	t.Setenv("XUI_DB_DSN", dsn)
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := database.InitDB(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	suffix := uuid.NewString()
	r := model.Reseller{Username: "voucher-" + suffix, Password: "hash", Enable: true, TrafficLimit: 100}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	v := model.ResellerVoucher{Code: "V-" + suffix, Amount: 10, TrafficGB: 1, MaxUses: 2}
	if err := db.Create(&v).Error; err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; _, _, err := (&VoucherService{}).Redeem(r.Id, v.Code); results <- err }()
	}
	close(start)
	a, b := <-results, <-results
	if (a == nil) == (b == nil) {
		t.Fatalf("expected exactly one redemption, got %v and %v", a, b)
	}
	if err := db.First(&r, r.Id).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&v, v.Id).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ResellerTransaction{}).Where("reseller_id = ?", r.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if r.Deposit != 10 || r.TrafficLimit != 100+bytesPerGB || v.UsedCount != 1 || count != 1 {
		t.Fatalf("duplicate or missing accounting: reseller=%+v voucher=%+v ledger=%d", r, v, count)
	}
}
