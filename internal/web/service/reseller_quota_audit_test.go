package service

import (
	"math"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestResellerQuotaBoundariesUseAllocatedAndMeasuredBytesSafely(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	reseller := model.Reseller{
		Username:     "quota-audit",
		Password:     "test-hash",
		Name:         "Quota audit",
		Enable:       true,
		TrafficLimit: 1000,
		ClientLimit:  1,
	}
	if err := db.Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	client := model.ClientRecord{Email: "quota-user@example.test", TotalGB: 500, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}).Error; err != nil {
		t.Fatal(err)
	}
	traffic := xray.ClientTraffic{Email: client.Email, Up: 200, Down: 300, Total: 500, Enable: true}
	if err := db.Create(&traffic).Error; err != nil {
		t.Fatal(err)
	}

	service := &ResellerService{}
	stat, err := service.Stat(&reseller)
	if err != nil {
		t.Fatal(err)
	}
	if stat.AllocatedTraffic != 500 || stat.UsedTraffic != 500 || stat.RemainingTraffic != 500 || stat.ClientCount != 1 || stat.OverQuota {
		t.Fatalf("unexpected in-quota snapshot: %+v", stat)
	}
	if err := service.CheckTrafficQuota(&reseller, 500); err != nil {
		t.Fatalf("allocating exactly the remaining bytes should be allowed: %v", err)
	}
	if err := service.CheckTrafficQuota(&reseller, 501); err == nil {
		t.Fatal("allocation one byte above the remaining quota should fail")
	}
	if err := service.CheckClientQuota(&reseller, 1, 0); err == nil {
		t.Fatal("adding a second client at a one-client limit should fail")
	}

	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Updates(map[string]any{"up": 700, "down": 300}).Error; err != nil {
		t.Fatal(err)
	}
	stat, err = service.Stat(&reseller)
	if err != nil {
		t.Fatal(err)
	}
	if stat.UsedTraffic != reseller.TrafficLimit || !stat.OverQuota {
		t.Fatalf("usage exactly at the traffic limit must be exhausted: used=%d overQuota=%v", stat.UsedTraffic, stat.OverQuota)
	}
	if err := service.CheckTrafficQuota(&reseller, 0); err == nil {
		t.Fatal("no additional allocation should be accepted after measured usage exhausts quota")
	}
}

func TestResellerQuotaChecksRejectNilAccount(t *testing.T) {
	service := &ResellerService{}
	if err := service.CheckTrafficQuota(nil, 1); err == nil {
		t.Fatal("traffic quota check should reject a nil reseller")
	}
	if err := service.CheckClientQuota(nil, 1, 1); err == nil {
		t.Fatal("client quota check should reject a nil reseller")
	}
}

func TestQuotaChecksRejectNegativeIncrementsAndMissingSnapshots(t *testing.T) {
	service := &ResellerService{}
	reseller := &model.Reseller{}
	if err := service.CheckTrafficQuota(reseller, -1); err == nil {
		t.Fatal("negative traffic allocation must be rejected even when unlimited")
	}
	if err := service.CheckClientQuota(reseller, -1, 0); err == nil {
		t.Fatal("negative client count increment must be rejected")
	}
	if err := service.CheckClientQuota(reseller, 0, -1); err == nil {
		t.Fatal("negative client traffic allocation must be rejected")
	}
	if err := checkTrafficQuotaSnapshot(reseller, nil, 0); err == nil {
		t.Fatal("missing quota snapshot must be rejected")
	}
}

func TestQuotaPercentDoesNotOverflowNearInt64Limit(t *testing.T) {
	if got := quotaPercent(math.MaxInt64, math.MaxInt64); got != 100 {
		t.Fatalf("at-limit percentage = %d, want 100", got)
	}
	if got := quotaPercent(math.MaxInt64-1, math.MaxInt64); got != 99 {
		t.Fatalf("one byte below limit percentage = %d, want 99", got)
	}
	if got := quotaPercent(math.MaxInt64, 1); got != 100 {
		t.Fatalf("over-limit percentage should be clamped to 100, got %d", got)
	}
}

func TestResellerQuotaArithmeticSaturatesAtInt64Limit(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	reseller := model.Reseller{Username: "quota-overflow", Password: "test-hash", Enable: true, TrafficLimit: math.MaxInt64}
	if err := db.Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	client := model.ClientRecord{Email: "overflow@example.test", TotalGB: math.MaxInt64, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{Email: client.Email, Up: math.MaxInt64, Down: 10, Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	service := &ResellerService{}
	stat, err := service.Stat(&reseller)
	if err != nil {
		t.Fatal(err)
	}
	if stat.AllocatedTraffic != math.MaxInt64 || stat.UsedTraffic != math.MaxInt64 || !stat.OverQuota {
		t.Fatalf("overflow must saturate and fail closed, got allocated=%d used=%d overQuota=%v", stat.AllocatedTraffic, stat.UsedTraffic, stat.OverQuota)
	}
	if err := service.CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("allocation at the integer maximum must not wrap around and be accepted")
	}
}
