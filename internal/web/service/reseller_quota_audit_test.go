package service

import (
	"math"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
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

func TestValidateResellerAllocationUnlimitedAndNegative(t *testing.T) {
	for _, tc := range []struct {
		limit, allocation int64
		wantErr           bool
	}{
		{100, 0, true}, {100, -1, true}, {100, 1, false},
		{0, 0, false}, {0, -1, true}, {0, 100, false},
	} {
		err := ValidateResellerAllocation(&model.Reseller{TrafficLimit: tc.limit}, tc.allocation)
		if (err != nil) != tc.wantErr {
			t.Fatalf("limit=%d allocation=%d: got %v, wantErr=%v", tc.limit, tc.allocation, err, tc.wantErr)
		}
	}
	if ValidateResellerAllocation(nil, 1) == nil {
		t.Fatal("missing reseller must fail")
	}
}

func TestResellerAllocationNeverFallsBelowEachClientsMeasuredUsage(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	reseller := model.Reseller{Username: "quota-reduction", Password: "hash", Enable: true, TrafficLimit: 1000}
	if err := db.Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	clients := []model.ClientRecord{
		{Email: "consumed@example.test", TotalGB: 100, Enable: true},
		{Email: "unused@example.test", TotalGB: 400, Enable: true},
	}
	for i := range clients {
		if err := db.Create(&clients[i]).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: clients[i].Email}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&xray.ClientTraffic{Email: clients[0].Email, Up: 100, Down: 200}).Error; err != nil {
		t.Fatal(err)
	}
	service := &ResellerService{}
	stat, err := service.Stat(&reseller)
	if err != nil || stat.UsedTraffic != 300 || stat.AllocatedTraffic != 700 || stat.RemainingTraffic != 300 {
		t.Fatalf("quota reduction released consumed bytes: %+v %v", stat, err)
	}
	if err := service.CheckTrafficQuota(&reseller, 301); err == nil {
		t.Fatal("consumed bytes must constrain new allocations")
	}
	if err := archiveResellerClientUsage(&clients[0]); err != nil {
		t.Fatal(err)
	}
	stat, err = service.Stat(&reseller)
	if err != nil || stat.AllocatedTraffic != 700 || stat.UsedTraffic != 300 {
		t.Fatalf("live usage double-counted after archive: %+v %v", stat, err)
	}
}

func TestResellerQuotaIncludesGlobalOnlyTrafficAndFailsClosed(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "global-only-quota", Password: "hash", Enable: true, TrafficLimit: 1000}
	c := model.ClientRecord{Email: "global-only@example.test", Enable: true, TotalGB: 100}
	for _, v := range []any{&r, &c} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []any{&model.ResellerClient{ResellerId: r.Id, Email: c.Email}, &model.ClientGlobalTraffic{MasterGuid: "master", Email: c.Email, Up: 400, Down: 300}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	svc := &ResellerService{}
	stat, err := svc.Stat(&r)
	if err != nil || stat.UsedTraffic != 700 || stat.AllocatedTraffic != 700 {
		t.Fatalf("global-only usage: %+v %v", stat, err)
	}
	if err := svc.CheckTrafficQuota(&r, 301); err == nil {
		t.Fatal("global usage was not charged")
	}
	if err := db.Migrator().DropTable(&model.ClientGlobalTraffic{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckTrafficQuota(&r, 1); err == nil {
		t.Fatal("quota check must fail when global traffic cannot be read")
	}
}

// Case variants can coexist under the exact-email unique index. Treat them as
// ambiguous rather than overlaying one master total onto each local row.
func TestAmbiguousTrafficFailsClosedForQuotaRenewalAndArchive(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "ambiguous-traffic", Password: "hash", Enable: true, TrafficLimit: 1000}
	c := model.ClientRecord{Email: "example@test", Enable: true, TotalGB: 100}
	for _, v := range []any{&r, &c} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []any{&model.ResellerClient{ResellerId: r.Id, Email: c.Email}, &xray.ClientTraffic{Email: c.Email, Up: 10}, &xray.ClientTraffic{Email: " Example@Test ", Up: 20}, &model.ClientGlobalTraffic{MasterGuid: "master", Email: c.Email, Up: 100}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	svc := &ResellerService{}
	if _, err := svc.Stat(&r); err == nil {
		t.Fatal("quota snapshot accepted ambiguous counters")
	}
	if err := svc.CheckTrafficQuota(&r, 1); err == nil {
		t.Fatal("quota allocation accepted ambiguous counters")
	}
	if err := archiveResellerClientUsage(&c); err == nil {
		t.Fatal("archive accepted ambiguous counters")
	}
	var count int64
	if err := db.Model(&model.ResellerTrafficArchive{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("archive unexpectedly changed: %d %v", count, err)
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		_, err := (&renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}).load(r.Id)
		return err
	})
	if err == nil {
		t.Fatal("renewal accepted ambiguous counters")
	}
}

func TestResellerQuotaCountsMixedCaseClientAllocation(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "mixed-case-quota", Password: "hash", Enable: true, TrafficLimit: 1000}
	c := model.ClientRecord{Email: "Mixed.Case@Example.Test", TotalGB: 700, Enable: true}
	for _, v := range []any{&r, &c} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.ResellerClient{ResellerId: r.Id, Email: c.Email}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ResellerService{}
	stat, err := svc.Stat(&r)
	if err != nil || stat.AllocatedTraffic != 700 || stat.RemainingTraffic != 300 {
		t.Fatalf("mixed-case allocation not counted: %+v %v", stat, err)
	}
	if err := svc.CheckTrafficQuota(&r, 301); err == nil {
		t.Fatal("allocation exceeding remaining quota accepted")
	}
	if err := svc.CheckTrafficQuota(&r, 300); err != nil {
		t.Fatalf("exact remaining allocation rejected: %v", err)
	}
}
