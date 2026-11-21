package service

import (
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
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

func TestInterruptedClientRenewalBlocksSubsequentOrdersAndResellerQuota(t *testing.T) {
	r, client := seedResellerRenewal(t)
	db := database.GetDB()
	first := model.PaymentOrder{OrderId: "first-ambiguous", TargetType: "client_renew", TargetRef: client.Email, Amount: 10, Status: "processing"}
	second := model.PaymentOrder{OrderId: "second-payment", TargetType: "client_renew", TargetRef: strings.ToUpper(client.Email), Amount: 10, AddDays: 1, Status: "pending"}
	for _, v := range []any{&first, &second} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	before, err := (&ClientService{}).GetRecordByEmail(nil, client.Email)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&PaymentWebhookService{}).ProcessWebhook(PaymentWebhookPayload{OrderId: second.OrderId, Amount: 10}); err == nil || !strings.Contains(err.Error(), "reconciliation") {
		t.Fatalf("second payment not blocked: %v", err)
	}
	var reloaded model.PaymentOrder
	if err := db.First(&reloaded, second.Id).Error; err != nil || reloaded.Status != "pending" {
		t.Fatalf("second order changed: %+v %v", reloaded, err)
	}
	after, err := (&ClientService{}).GetRecordByEmail(nil, client.Email)
	if err != nil || after.ExpiryTime != before.ExpiryTime {
		t.Fatalf("client changed: %+v %v", after, err)
	}
	if err := (&ResellerService{}).CheckClientQuota(&r, 1, 1); err == nil {
		t.Fatal("allocation allowed with ambiguous renewal")
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		_, err := (&renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}).load(r.Id)
		return err
	})
	if err == nil {
		t.Fatal("automatic renewal allowed with ambiguous renewal")
	}
}
