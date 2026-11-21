package service

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestResellerSignupSignedLinkAndTemplateScope(t *testing.T) {
	dir := t.TempDir()
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })

	db := database.GetDB()
	var inboundSvc InboundService
	inbound, _, err := inboundSvc.AddInbound(&model.Inbound{
		Port: 20443, Protocol: model.VLESS,
		Settings:       `{"clients":[],"decryption":"none"}`,
		StreamSettings: `{"network":"tcp","security":"none"}`,
		Tag:            "signup-inbound", Sniffing: `{"enabled":true,"destOverride":["http","tls"]}`,
	})
	if err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	reseller := &model.Reseller{Username: "signup-reseller", Password: "test-password", Name: "Signup Shop", BrandName: "Northstar", BrandLogo: "/assets/northstar.svg", BrandColor: "#224466", Enable: true, ClientLimit: 1}
	if err := db.Create(reseller).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	template := &model.UserTemplate{
		Name: "Signup Basic", TotalGB: bytesPerGB * 10, ExpireDurationDays: 30,
		InboundIds: []int{inbound.Id},
	}
	if err := db.Create(template).Error; err != nil {
		t.Fatal(err)
	}
	outsideTemplate := &model.UserTemplate{Name: "Out of scope", InboundIds: []int{5}}
	if err := db.Create(outsideTemplate).Error; err != nil {
		t.Fatal(err)
	}

	svc := &ResellerSignupService{}
	secret := []byte("test-signing-secret")
	now := time.Unix(1_800_000_000, 0)
	token, err := svc.CreateLink(reseller.Id, template.Id, secret, now)
	if err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if strings.Count(token, ".") != 1 {
		t.Fatalf("token should contain encoded payload and signature, got %q", token)
	}
	info, err := svc.Info(token, secret, now)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.ResellerName != "Signup Shop" || info.BrandName != "Northstar" || info.BrandLogo != "/assets/northstar.svg" || info.BrandColor != "#224466" || info.PlanName != "Signup Basic" || info.TotalBytes != 10*bytesPerGB {
		t.Fatalf("unexpected public info: %+v", info)
	}

	eligible, err := svc.EligibleTemplates(reseller.Id)
	if err != nil {
		t.Fatal(err)
	}
	if len(eligible) != 1 || eligible[0].Id != template.Id {
		t.Fatalf("eligible templates = %+v, expected only reseller-owned plan", eligible)
	}
	subID, needRestart, err := svc.Register(token, "Customer@Example.com", secret, now)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if subID == "" || !needRestart {
		t.Fatalf("Register returned subId=%q needRestart=%v", subID, needRestart)
	}
	var clientSvc ClientService
	created, err := clientSvc.GetRecordByEmail(nil, "customer@example.com")
	if err != nil || created.TotalGB != template.TotalGB || created.ExpiryTime != now.Add(30*24*time.Hour).UnixMilli() {
		t.Fatalf("created client does not match plan: %+v (%v)", created, err)
	}
	owned, err := (&ResellerService{}).OwnsClient(reseller.Id, "customer@example.com")
	if err != nil || !owned {
		t.Fatalf("new client should be assigned to reseller: owned=%v err=%v", owned, err)
	}
	if _, _, err := svc.Register(token, "another@example.com", secret, now); err == nil {
		t.Fatal("expected client limit to block additional registrations")
	}
	if _, err := svc.CreateLink(reseller.Id, outsideTemplate.Id, secret, now); err == nil {
		t.Fatal("expected link generation for an unowned inbound template to fail")
	}
	if _, err := svc.Info(token+"x", secret, now); err == nil {
		t.Fatal("expected modified signature to fail")
	}
	if _, err := svc.Info(token, secret, now.Add(resellerSignupTTL+time.Second)); err == nil {
		t.Fatal("expected expired registration link to fail")
	}
}

// An interrupted or uncompensated public signup must fence subsequent
// allocations for the same reseller, even though signup has no panel session.
func TestResellerSignupPreservesIntentWhenCompensationCannotFinish(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "signup-intent", Password: "hash", Enable: true, TrafficLimit: 1000}
	in := model.Inbound{UserId: 1, Port: 19955, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "signup-intent-inbound"}
	for _, row := range []any{&r, &in} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: r.Id, InboundId: in.Id}).Error; err != nil {
		t.Fatal(err)
	}
	plan := model.UserTemplate{Name: "intent-signup-plan", TotalGB: 50, InboundIds: []int{in.Id}}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ResellerSignupService{}
	secret := []byte("intent-test-secret")
	now := time.Now()
	token, err := svc.CreateLink(r.Id, plan.Id, secret, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_signup_sale BEFORE INSERT ON reseller_sales BEGIN SELECT RAISE(ABORT, 'sale failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER reject_signup_intent_delete BEFORE DELETE ON reseller_creation_intents BEGIN SELECT RAISE(ABORT, 'intent failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Register(token, "first-signup@example.test", secret, now); err == nil {
		t.Fatal("expected signup failure")
	}
	var intents []model.ResellerCreationIntent
	if err := db.Where("reseller_id = ?", r.Id).Find(&intents).Error; err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].SubID == "" || intents[0].Email != "first-signup@example.test" {
		t.Fatalf("missing durable intent: %+v", intents)
	}
	if _, _, err := svc.Register(token, "second-signup@example.test", secret, now); err == nil || !strings.Contains(err.Error(), "reconciliation") {
		t.Fatalf("expected pending intent to block signup, got %v", err)
	}
	var count int64
	if err := db.Model(&model.ResellerSale{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unexpected sale count %d, %v", count, err)
	}
}
