package service

import (
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestAssignClientsWithSalesRecordsCreationAndQuotaAllocation(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	reseller := model.Reseller{Username: "sales-test", Password: "hash", Enable: true, TrafficLimit: 10_000}
	if err := db.Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2025, time.February, 3, 12, 30, 0, 0, time.UTC).UnixMilli()
	clients := []model.ClientRecord{
		{Email: "sales-one@example.test", TotalGB: 2_000, Enable: true, CreatedAt: createdAt},
		{Email: "sales-two@example.test", TotalGB: 3_000, Enable: true, CreatedAt: time.Date(2025, time.February, 14, 8, 0, 0, 0, time.UTC).UnixMilli()},
	}
	for i := range clients {
		if err := db.Create(&clients[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	service := &ResellerService{}
	if err := service.AssignClientsWithSales(reseller.Id,
		[]string{clients[0].Email, clients[1].Email}, []string{clients[0].Email, clients[1].Email}); err != nil {
		t.Fatal(err)
	}

	// The reseller's remaining quota is computed from assigned clients' actual
	// total_gb allocations, not from a display-only or stale counter.
	stat, err := service.Stat(&reseller)
	if err != nil {
		t.Fatal(err)
	}
	if stat.AllocatedTraffic != 5_000 || stat.RemainingTraffic != 5_000 {
		t.Fatalf("quota should include both created allocations: allocated=%d remaining=%d", stat.AllocatedTraffic, stat.RemainingTraffic)
	}

	// Retrying ownership assignment must not duplicate or rewrite sale snapshots.
	if err := service.AssignClientsWithSales(reseller.Id,
		[]string{clients[0].Email, clients[1].Email}, []string{clients[0].Email, clients[1].Email}); err != nil {
		t.Fatal(err)
	}
	report, err := service.SalesReport(reseller.Id, "monthly", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.TotalSales != 2 || report.TotalTrafficBytes != 5_000 || len(report.Buckets) != 1 || report.Buckets[0].Period != "2025-02" {
		t.Fatalf("unexpected monthly sales report after retry: %+v", report)
	}
	daily, err := service.SalesReport(reseller.Id, "daily", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily.Buckets) != 2 || daily.Buckets[0].Period != "2025-02-03" || daily.Buckets[1].Period != "2025-02-14" {
		t.Fatalf("unexpected daily buckets: %+v", daily.Buckets)
	}
	yearly, err := service.SalesReport(reseller.Id, "yearly", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(yearly.Buckets) != 1 || yearly.Buckets[0].Period != "2025" {
		t.Fatalf("unexpected yearly buckets: %+v", yearly.Buckets)
	}
}

func TestAssignClientsWithSalesRejectsUnassignedSalesAndRollsBack(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	reseller := model.Reseller{Username: "sales-rollback", Password: "hash", Enable: true}
	if err := db.Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	client := model.ClientRecord{Email: "rollback@example.test", TotalGB: 123, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	service := &ResellerService{}
	if err := service.AssignClientsWithSales(reseller.Id, []string{client.Email}, []string{"other@example.test"}); err == nil {
		t.Fatal("sale snapshot for an unassigned client should be rejected")
	}
	var mappings int64
	if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ?", reseller.Id).Count(&mappings).Error; err != nil {
		t.Fatal(err)
	}
	var sales int64
	if err := db.Model(&model.ResellerSale{}).Where("reseller_id = ?", reseller.Id).Count(&sales).Error; err != nil {
		t.Fatal(err)
	}
	if mappings != 0 || sales != 0 {
		t.Fatalf("rejected assignment must not persist data: mappings=%d sales=%d", mappings, sales)
	}
}

func TestResellerSalesReportValidatesPeriodAndRange(t *testing.T) {
	setupBulkDB(t)
	reseller := model.Reseller{Username: "sales-validation", Password: "hash", Enable: true}
	if err := database.GetDB().Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	service := &ResellerService{}
	for _, bounds := range []struct {
		period string
		from   int64
		to     int64
	}{
		{period: "weekly"},
		{period: "daily", from: 20, to: 10},
		{period: "daily", from: -1},
	} {
		if _, err := service.SalesReport(reseller.Id, bounds.period, bounds.from, bounds.to); err == nil {
			t.Fatalf("expected invalid report query to fail: %+v", bounds)
		}
	}
}
