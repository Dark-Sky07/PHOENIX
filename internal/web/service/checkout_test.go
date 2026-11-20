package service

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func enableCheckout(t *testing.T, pricePerGB, pricePerMonth float64) {
	t.Helper()
	var settingSvc SettingService
	if err := settingSvc.setBool("checkoutEnable", true); err != nil {
		t.Fatalf("setBool checkoutEnable: %v", err)
	}
	if err := settingSvc.setString("checkoutCurrency", "USD"); err != nil {
		t.Fatalf("setString checkoutCurrency: %v", err)
	}
	if err := settingSvc.setString("checkoutPricePerGB", strconv.FormatFloat(pricePerGB, 'f', -1, 64)); err != nil {
		t.Fatalf("set checkoutPricePerGB: %v", err)
	}
	if err := settingSvc.setString("checkoutPricePerMonth", strconv.FormatFloat(pricePerMonth, 'f', -1, 64)); err != nil {
		t.Fatalf("set checkoutPricePerMonth: %v", err)
	}
}

func seedCheckoutClient(t *testing.T) string {
	t.Helper()
	svc := &ClientService{}
	clients := []model.Client{
		{Email: "checkout-user@x", ID: "44444444-4444-4444-4444-444444444444", SubID: "schk", Enable: true, TotalGB: 1},
	}
	ib := mkInbound(t, 20011, model.VLESS, clientsSettings(t, clients))
	if err := svc.SyncInbound(nil, ib.Id, clients); err != nil {
		t.Fatalf("SyncInbound: %v", err)
	}
	return "checkout-user@x"
}

func TestCheckoutRefToken(t *testing.T) {
	secret := []byte("panel-secret")
	now := time.Now()

	// Round-trip, with case/trim normalization
	token, err := SignCheckoutRef("  Alice@X.COM ", secret, now)
	if err != nil {
		t.Fatalf("SignCheckoutRef: %v", err)
	}
	email, err := ParseCheckoutRef(token, secret, now)
	if err != nil || email != "alice@x.com" {
		t.Fatalf("ParseCheckoutRef round-trip: email=%q err=%v", email, err)
	}

	// Different secret must reject
	if _, err := ParseCheckoutRef(token, []byte("other-secret"), now); err != ErrCheckoutInvalidRef {
		t.Fatalf("expected ErrCheckoutInvalidRef for wrong secret, got %v", err)
	}

	// Tampered payload must reject
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("unexpected token shape: %q", token)
	}
	tampered := parts[0][:len(parts[0])-2] + "XX." + parts[1]
	if _, err := ParseCheckoutRef(tampered, secret, now); err == nil {
		t.Fatal("expected tampered payload to be rejected")
	}

	// Expired token must reject with ErrCheckoutExpiredRef
	old, err := SignCheckoutRef("alice@x.com", secret, now.Add(-366*24*time.Hour))
	if err != nil {
		t.Fatalf("SignCheckoutRef old: %v", err)
	}
	if _, err := ParseCheckoutRef(old, secret, now); err != ErrCheckoutExpiredRef {
		t.Fatalf("expected ErrCheckoutExpiredRef, got %v", err)
	}

	// Garbage tokens must reject
	for _, junk := range []string{"", "abc", "....", "a.b", token + "x"} {
		if _, err := ParseCheckoutRef(junk, secret, now); err == nil {
			t.Fatalf("expected junk token %q to be rejected", junk)
		}
	}

	// Empty email is refused at signing
	if _, err := SignCheckoutRef("   ", secret, now); err == nil {
		t.Fatal("expected empty email to fail signing")
	}
}

func TestCheckoutInfoDisabled(t *testing.T) {
	setupBulkDB(t) // fresh DB keeps checkoutEnable=false default
	var svc CheckoutService
	if _, err := svc.Info("anyone@x"); err != ErrCheckoutDisabled {
		t.Fatalf("expected ErrCheckoutDisabled on fresh settings, got %v", err)
	}
}

func TestCheckoutOrderAndWebhookCompletion(t *testing.T) {
	setupBulkDB(t)
	email := seedCheckoutClient(t)
	enableCheckout(t, 0.5, 4)

	var svc CheckoutService

	// Info returns plan state + pricing
	info, err := svc.Info(email)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Email != email || info.Currency != "USD" || info.PricePerGB != 0.5 || info.PricePerMonth != 4 {
		t.Fatalf("unexpected info: %+v", info)
	}

	// Validation: nothing selected
	if _, err := svc.CreateOrder(email, 0, 0); err == nil {
		t.Fatal("expected empty order to fail")
	}
	// Validation: out of range
	if _, err := svc.CreateOrder(email, 10001, 0); err == nil {
		t.Fatal("expected gb>10000 to fail")
	}
	if _, err := svc.CreateOrder(email, 0, 37); err == nil {
		t.Fatal("expected months>36 to fail")
	}
	// Unknown client
	if _, err := svc.CreateOrder("ghost@x", 10, 0); err == ErrCheckoutInvalidRef {
		// ok
	} else if err == nil {
		t.Fatal("expected ghost client order to fail")
	}

	// Happy path: 20GB + 1 month = 20*0.5 + 4 = 14
	order, err := svc.CreateOrder(email, 20, 1)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if order.Amount != 14 || !strings.HasPrefix(order.OrderId, "co_") || order.GatewayURL != "" {
		t.Fatalf("unexpected order: %+v", order)
	}

	// Status of the fresh order is pending
	status, err := svc.Status(email, order.OrderId)
	if err != nil || status != "pending" {
		t.Fatalf("Status: %q err=%v", status, err)
	}
	// Another client's order id reads as unknown
	if st, _ := svc.Status("someoneelse@x", order.OrderId); st != "unknown" {
		t.Fatalf("expected unknown for foreign order, got %q", st)
	}

	// Pending cap: 10 open orders max
	for i := 0; i < 9; i++ {
		if _, err := svc.CreateOrder(email, 1, 0); err != nil {
			t.Fatalf("CreateOrder #%d: %v", i+2, err)
		}
	}
	if _, err := svc.CreateOrder(email, 1, 0); err == nil {
		t.Fatal("expected 11th pending order to be refused")
	}

	// Amount mismatch on webhook completion is refused
	var webhookSvc PaymentWebhookService
	if _, err := webhookSvc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 999}); err == nil {
		t.Fatal("expected amount mismatch to fail")
	}

	// Webhook completes the pending order using only orderId+amount
	completed, err := webhookSvc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 14})
	if err != nil {
		t.Fatalf("ProcessWebhook complete: %v", err)
	}
	if completed.Status != "completed" {
		t.Fatalf("expected completed status, got %q", completed.Status)
	}

	// Effects were applied: +20GB quota, +30d expiry, enabled
	var clientSvc ClientService
	rec, err := clientSvc.GetRecordByEmail(nil, email)
	if err != nil {
		t.Fatal(err)
	}
	if rec.TotalGB != 20*bytesPerGB+1 {
		t.Fatalf("expected quota 20GB in bytes, got %d", rec.TotalGB)
	}
	if !rec.Enable {
		t.Fatal("expected client to be enabled after renewal")
	}
	if rec.ExpiryTime <= time.Now().UnixMilli() {
		t.Fatalf("expected future expiry, got %d", rec.ExpiryTime)
	}

	// Second webhook with same order id is idempotent: no double quota
	dup, err := webhookSvc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 14})
	if err != nil || dup.Id != completed.Id || dup.Status != "completed" {
		t.Fatalf("expected idempotent completed return, got %+v (%v)", dup, err)
	}
	rec2, _ := clientSvc.GetRecordByEmail(nil, email)
	if rec2.TotalGB != 20*bytesPerGB+1 {
		t.Fatalf("expected quota unchanged after duplicate webhook, got %d", rec2.TotalGB)
	}

	// Gateway URL placeholders get substituted
	var settingSvc SettingService
	if err := settingSvc.setString("checkoutGatewayURL", "https://pay.example.com/i?id={orderId}&a={amount}&r={ref}"); err != nil {
		t.Fatal(err)
	}
	order2, err := svc.CreateOrder(email, 10, 0)
	if err != nil {
		t.Fatalf("CreateOrder with gateway: %v", err)
	}
	want := "https://pay.example.com/i?id=" + order2.OrderId + "&a=5&r=" + email
	if order2.GatewayURL != want {
		t.Fatalf("gateway URL: got %q want %q", order2.GatewayURL, want)
	}
}

func TestCheckoutPendingOrderSurvivesDisabledCheckout(t *testing.T) {
	setupBulkDB(t)
	email := seedCheckoutClient(t)
	enableCheckout(t, 1, 2)

	var svc CheckoutService
	order, err := svc.CreateOrder(email, 5, 0)
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}

	// Even after checkout is disabled again, a paid order still completes:
	// the gateway callback is proof of payment, not a new order.
	var settingSvc SettingService
	if err := settingSvc.setBool("checkoutEnable", false); err != nil {
		t.Fatal(err)
	}
	var webhookSvc PaymentWebhookService
	completed, err := webhookSvc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 5})
	if err != nil {
		t.Fatalf("ProcessWebhook after disable: %v", err)
	}
	if completed.Status != "completed" {
		t.Fatalf("expected completed, got %q", completed.Status)
	}
}

func TestCheckoutFreshWebhookStillNeedsTargetRef(t *testing.T) {
	setupBulkDB(t)
	var webhookSvc PaymentWebhookService
	if _, err := webhookSvc.ProcessWebhook(PaymentWebhookPayload{OrderId: "ORD-NEVER-SEEN"}); err == nil {
		t.Fatal("expected unknown order without targetRef to be refused")
	}
}

func TestCheckoutDoesNotSellTrafficToUnlimitedClientOrInterruptedRenewal(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	client := model.ClientRecord{Email: "Unlimited@Example.test", Enable: true, TotalGB: 0}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	enableCheckout(t, 1, 2)
	svc := &CheckoutService{}
	if _, err := svc.CreateOrder(client.Email, 1, 0); err == nil {
		t.Fatal("sold traffic which would turn unlimited into finite")
	}
	var n int64
	if err := db.Model(&model.PaymentOrder{}).Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("rejected checkout created order: %d %v", n, err)
	}
	order := model.PaymentOrder{OrderId: "unlimited-existing", TargetType: "client_renew", TargetRef: client.Email, AddGB: 1, Amount: 1, Status: "pending"}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&PaymentWebhookService{}).ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 1}); err == nil {
		t.Fatal("existing order converted unlimited quota")
	}
	if err := db.First(&order, order.Id).Error; err != nil || order.Status != "pending" {
		t.Fatalf("order changed: %+v %v", order, err)
	}
	processing := model.PaymentOrder{OrderId: "ambiguous-processing", TargetType: "client_renew", TargetRef: strings.ToLower(client.Email), Status: "processing"}
	if err := db.Create(&processing).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateOrder(client.Email, 0, 1); err == nil {
		t.Fatal("sold renewal despite ambiguous prior payment")
	}
}

func TestCheckoutPendingCapUsesNormalizedEmail(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	client := model.ClientRecord{Email: "Case@Example.test", Enable: true, TotalGB: 1}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	enableCheckout(t, 1, 1)
	svc := &CheckoutService{}
	for i := 0; i < checkoutMaxPendingOrders; i++ {
		if _, err := svc.CreateOrder("CASE@example.test", 1, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.CreateOrder("case@example.test", 1, 0); err == nil {
		t.Fatal("case variants bypassed pending order limit")
	}
	var order model.PaymentOrder
	if err := db.Where("target_type = ?", "client_renew").First(&order).Error; err != nil {
		t.Fatal(err)
	}
	if status, err := svc.Status(" CASE@example.test ", order.OrderId); err != nil || status != "pending" {
		t.Fatalf("normalized order status: %q %v", status, err)
	}
}

func TestConcurrentCheckoutPendingOrderLimit(t *testing.T) {
	setupBulkDB(t)
	email := seedCheckoutClient(t)
	enableCheckout(t, 1, 1)
	start := make(chan struct{})
	results := make(chan error, checkoutMaxPendingOrders+5)
	for i := 0; i < cap(results); i++ {
		go func() { <-start; _, err := (&CheckoutService{}).CreateOrder(email, 1, 0); results <- err }()
	}
	close(start)
	successes := 0
	for i := 0; i < cap(results); i++ {
		if <-results == nil {
			successes++
		}
	}
	var n int64
	if err := database.GetDB().Model(&model.PaymentOrder{}).Where("target_type = ? AND status = ?", "client_renew", "pending").Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if successes != checkoutMaxPendingOrders || n != checkoutMaxPendingOrders {
		t.Fatalf("pending checkout cap bypassed: successes=%d rows=%d", successes, n)
	}
}

func TestCheckoutRejectsUnfulfillableResellerRenewal(t *testing.T) {
	r, c := seedResellerRenewal(t)
	db := database.GetDB()
	enableCheckout(t, 1, 1)
	svc := &CheckoutService{}
	if _, err := svc.CreateOrder(c.Email, 1, 0); err == nil {
		t.Fatal("sold traffic beyond reseller quota")
	}
	var n int64
	if err := db.Model(&model.PaymentOrder{}).Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("unexpected order: %d %v", n, err)
	}
	if err := db.Model(&r).Update("traffic_limit", bytesPerGB+500).Error; err != nil {
		t.Fatal(err)
	}
	intent := model.ResellerCreationIntent{Id: "checkout-unresolved", ResellerId: r.Id, Email: "other@example.test", SubID: "other", InboundIDs: "[]"}
	if err := db.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateOrder(c.Email, 1, 0); err == nil {
		t.Fatal("sold renewal while creation requires reconciliation")
	}
	if err := db.Delete(&intent).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateOrder(c.Email, 1, 0); err != nil {
		t.Fatalf("exact fit should be sellable: %v", err)
	}
}

func TestPricedPaymentOrdersRequireConfirmedAmount(t *testing.T) {
	for _, target := range []string{"client_renew", "reseller_deposit", "reseller_quota"} {
		t.Run(target, func(t *testing.T) {
			setupBulkDB(t)
			db := database.GetDB()
			email := seedCheckoutClient(t)
			reseller := model.Reseller{Username: "amount-check", Password: "hash", Enable: true, TrafficLimit: bytesPerGB}
			if err := db.Create(&reseller).Error; err != nil {
				t.Fatal(err)
			}
			ref := reseller.Username
			if target == "client_renew" {
				ref = email
			}
			order := model.PaymentOrder{OrderId: "amount-" + target, TargetType: target, TargetRef: ref, Amount: 10, AddDays: 1, Status: "pending"}
			if err := db.Create(&order).Error; err != nil {
				t.Fatal(err)
			}
			svc := &PaymentWebhookService{}
			for _, amount := range []float64{0, -1, 9} {
				if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: amount}); err == nil {
					t.Fatalf("accepted amount %v", amount)
				}
				if err := db.First(&order, order.Id).Error; err != nil || order.Status != "pending" {
					t.Fatalf("order changed: %+v %v", order, err)
				}
			}
			if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err != nil {
				t.Fatalf("correct amount rejected: %v", err)
			}
		})
	}
}

func TestCheckoutCurrencySnapshotSurvivesSettingChange(t *testing.T) {
	setupBulkDB(t)
	email := seedCheckoutClient(t)
	enableCheckout(t, 10, 1)
	order, err := (&CheckoutService{}).CreateOrder(email, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	var stored model.PaymentOrder
	if err := db.Where("order_id = ?", order.OrderId).First(&stored).Error; err != nil || stored.Currency != "USD" {
		t.Fatalf("currency snapshot: %+v %v", stored, err)
	}
	if err := (&SettingService{}).setString("checkoutCurrency", "JPY"); err != nil {
		t.Fatal(err)
	}
	svc := &PaymentWebhookService{}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10.1}); err == nil {
		t.Fatal("used changed currency tolerance for old order")
	}
	if err := db.First(&stored, stored.Id).Error; err != nil || stored.Status != "pending" {
		t.Fatalf("changed order: %+v %v", stored, err)
	}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err != nil {
		t.Fatalf("valid payment rejected: %v", err)
	}
}

func TestDepositCurrencyChangeRequiresReconciliation(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	enableCheckout(t, 1, 1)
	r := model.Reseller{Username: "currency-check", Password: "hash", Enable: true, TrafficLimit: 1}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	order := model.PaymentOrder{OrderId: "currency-deposit", TargetType: "reseller_deposit", TargetRef: r.Username, Amount: 10, Currency: "USD", Status: "pending"}
	if err := db.Create(&order).Error; err != nil {
		t.Fatal(err)
	}
	settings := &SettingService{}
	if err := settings.setString("checkoutCurrency", "JPY"); err != nil {
		t.Fatal(err)
	}
	svc := &PaymentWebhookService{}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err == nil {
		t.Fatal("credited deposit in a different currency")
	}
	if err := db.First(&order, order.Id).Error; err != nil || order.Status != "pending" {
		t.Fatalf("changed order: %+v %v", order, err)
	}
	if err := db.First(&r, r.Id).Error; err != nil || r.Deposit != 0 {
		t.Fatalf("changed deposit: %+v %v", r, err)
	}
	if err := settings.setString("checkoutCurrency", "USD"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessWebhook(PaymentWebhookPayload{OrderId: order.OrderId, Amount: 10}); err != nil {
		t.Fatalf("original currency rejected: %v", err)
	}
}
