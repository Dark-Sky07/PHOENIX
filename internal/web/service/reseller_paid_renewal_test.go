package service

import (
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"testing"
)

func TestPaidResellerClientRenewalCannotExceedQuota(t *testing.T) {
	r, c := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Model(&r).Update("traffic_limit", 799).Error; err != nil {
		t.Fatal(err)
	}
	order := model.PaymentOrder{OrderId: "reseller-paid-renewal", TargetType: "client_renew", TargetRef: c.Email, Amount: 10, ResetTraffic: true, Status: "pending"}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	_, err := (&PaymentWebhookService{}).ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10})
	if err == nil {
		t.Fatal("renewal beyond reseller limit succeeded")
	}
	var traffic xray.ClientTraffic
	if err := db.Where("email = ?", c.Email).First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&order, order.Id).Error; err != nil {
		t.Fatal(err)
	}
	if traffic.Up != 120 || traffic.Down != 180 || order.Status != "pending" {
		t.Fatalf("rejected renewal changed counters or order: %+v %+v", traffic, order)
	}
	if err := db.Model(&r).Update("traffic_limit", 800).Error; err != nil {
		t.Fatal(err)
	}
	if err := validatePaidClientRenewal(&c, 500, true); err != nil {
		t.Fatalf("exact-fit renewal rejected: %v", err)
	}
	if err := validatePaidClientRenewal(&c, 501, true); err == nil {
		t.Fatal("quota increase beyond limit accepted")
	}
}
