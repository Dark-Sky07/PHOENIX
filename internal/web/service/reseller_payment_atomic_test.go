package service

import (
	"math"
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/google/uuid"
)

func TestResellerPaymentRollsBackAllEffects(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "atomic-pay", Password: "hash", Enable: true, TrafficLimit: 100}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	order := model.PaymentOrder{OrderId: "atomic-pay-order", TargetType: "reseller_deposit", TargetRef: r.Username, Amount: 10, AddGB: 1, Status: "pending"}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_payment_completion BEFORE UPDATE OF status ON payment_orders BEGIN SELECT RAISE(ABORT, 'injected failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	svc := &PaymentWebhookService{}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err == nil {
		t.Fatal("expected injected failure")
	}
	if err := db.First(&r, r.Id).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&order, order.Id).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ResellerTransaction{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if r.Deposit != 0 || r.TrafficLimit != 100 || order.Status != "pending" || count != 0 {
		t.Fatalf("partial payment committed: %+v %+v ledger=%d", r, order, count)
	}
	if err := db.Exec("DROP TRIGGER reject_payment_completion").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&r, r.Id).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ResellerTransaction{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if r.Deposit != 10 || r.TrafficLimit != 100+bytesPerGB || count != 1 {
		t.Fatalf("incorrect payment: %+v ledger=%d", r, count)
	}
}

func TestResellerPaymentRejectsQuotaOverflow(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "payment-overflow", Password: "hash", Enable: true, TrafficLimit: math.MaxInt64 - 1}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	_, err := (&PaymentWebhookService{}).ProcessWebhook(PaymentWebhookPayload{OrderId: "overflow-order", TargetType: "reseller_deposit", TargetRef: r.Username, Amount: 1, AddGB: 1})
	if err == nil {
		t.Fatal("expected quota overflow to fail")
	}
	if err := db.First(&r, r.Id).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.PaymentOrder{}).Where("order_id = ?", "overflow-order").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if r.TrafficLimit != math.MaxInt64-1 || r.Deposit != 0 || count != 0 {
		t.Fatalf("overflow changed account: %+v orders=%d", r, count)
	}
}

func TestConcurrentResellerPaymentPostgres(t *testing.T) {
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
	id := uuid.NewString()
	r := model.Reseller{Username: "payment-" + id, Password: "hash", Enable: true, TrafficLimit: 100}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	order := model.PaymentOrder{OrderId: "pay-" + id, TargetType: "reseller_deposit", TargetRef: r.Username, Amount: 10, AddGB: 1, Status: "pending"}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, err := (&PaymentWebhookService{}).ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10})
			results <- err
		}()
	}
	close(start)
	if a, b := <-results, <-results; a != nil || b != nil {
		t.Fatalf("callbacks: %v, %v", a, b)
	}
	if err := db.First(&r, r.Id).Error; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ResellerTransaction{}).Where("reseller_id = ?", r.Id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if r.Deposit != 10 || r.TrafficLimit != 100+bytesPerGB || count != 1 {
		t.Fatalf("duplicate payment: %+v ledger=%d", r, count)
	}
}
