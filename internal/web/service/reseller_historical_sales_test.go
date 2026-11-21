package service

import (
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestHistoricalSaleRecordsSourceAndCountsOnce(t *testing.T) {
	setupBulkDB(t)
	reseller := model.Reseller{Username: "history", Password: "hash", Enable: true}
	if err := database.GetDB().Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	amount := int64(1000)
	row := model.ResellerHistoricalSale{ResellerId: reseller.Id, Reference: "invoice-42", Source: "archive invoice 42", Email: "old@example.test", SoldAt: time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC).UnixMilli(), TrafficBytes: 200, AmountMinor: &amount, Currency: "irr", ActorType: "admin", ActorId: 1, ActorName: "admin"}
	svc := &ResellerService{}
	if err := svc.RecordHistoricalSale(&row); err != nil {
		t.Fatal(err)
	}
	duplicate := row
	if err := svc.RecordHistoricalSale(&duplicate); err == nil {
		t.Fatal("duplicate reference accepted")
	}
	rows, err := svc.HistoricalSales(reseller.Id)
	if err != nil || len(rows) != 1 || rows[0].Source != "archive invoice 42" || rows[0].ActorId != 1 || rows[0].Currency != "IRR" {
		t.Fatalf("unexpected history: %+v %v", rows, err)
	}
	report, err := svc.SalesReport(reseller.Id, "yearly", 0, 0)
	if err != nil || report.TotalSales != 1 || report.HistoricalSales != 1 || report.AmountsMinor["IRR"] != "1000" || len(report.Buckets) != 1 || report.Buckets[0].Period != "2024" {
		t.Fatalf("unexpected report: %+v %v", report, err)
	}
	filtered, err := svc.SalesReport(reseller.Id, "monthly", row.SoldAt+1, 0)
	if err != nil || filtered.TotalSales != 0 {
		t.Fatalf("unexpected range filter: %+v %v", filtered, err)
	}
	row.Reference = "invoice-43"
	row.ActorType = "reseller"
	if err := svc.RecordHistoricalSale(&row); err == nil {
		t.Fatal("reseller actor accepted")
	}
}
