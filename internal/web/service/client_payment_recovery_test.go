package service

import (
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestClientRenewalAmbiguousCompletionIsNotReplayed(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	order := model.PaymentOrder{OrderId: "uncertain-client-payment", TargetType: "client_renew", TargetRef: client.Email, Amount: 10, AddDays: 1, Status: "pending"}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_client_completion BEFORE UPDATE OF status ON payment_orders WHEN NEW.status = 'completed' BEGIN SELECT RAISE(ABORT, 'injected completion error'); END`).Error; err != nil {
		t.Fatal(err)
	}
	svc := &PaymentWebhookService{}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err == nil || !strings.Contains(err.Error(), "manual reconciliation") {
		t.Fatalf("expected ambiguous completion error, got %v", err)
	}
	if err := db.First(&order, order.Id).Error; err != nil {
		t.Fatal(err)
	}
	if order.Status != "processing" {
		t.Fatalf("status = %q", order.Status)
	}
	first, err := (&ClientService{}).GetRecordByEmail(nil, client.Email)
	if err != nil {
		t.Fatal(err)
	}
	if first.ExpiryTime <= 0 {
		t.Fatalf("renewal did not execute before injected failure: %+v", first)
	}
	if err := db.Exec("DROP TRIGGER reject_client_completion").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err == nil || !strings.Contains(err.Error(), "manual reconciliation") {
		t.Fatalf("expected replay refusal, got %v", err)
	}
	second, err := (&ClientService{}).GetRecordByEmail(nil, client.Email)
	if err != nil {
		t.Fatal(err)
	}
	if second.ExpiryTime != first.ExpiryTime {
		t.Fatalf("replay changed expiry: %d -> %d", first.ExpiryTime, second.ExpiryTime)
	}
}
