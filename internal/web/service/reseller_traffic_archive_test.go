package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestResellerUsageSurvivesClientDeletion(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	reseller := model.Reseller{Username: "deleted-traffic", Password: "hash", Enable: true, TrafficLimit: 1000}
	if err := db.Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	client := model.ClientRecord{Email: "deleted@example.test", TotalGB: 500, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{Email: client.Email, Up: 120, Down: 180, Total: 500}).Error; err != nil {
		t.Fatal(err)
	}
	service := &ResellerService{}
	if err := archiveResellerClientUsage(&client); err != nil {
		t.Fatal(err)
	}
	if err := archiveResellerClientUsage(&client); err != nil {
		t.Fatal(err)
	}
	stat, err := service.Stat(&reseller)
	if err != nil || stat.UsedTraffic != 300 || stat.AllocatedTraffic != 500 {
		t.Fatalf("live usage counted twice: %+v %v", stat, err)
	}
	clients := &ClientService{}
	if _, err := clients.Delete(&InboundService{}, client.Id, false); err != nil {
		t.Fatal(err)
	}
	stat, err = service.Stat(&reseller)
	if err != nil || stat.UsedTraffic != 300 || stat.AllocatedTraffic != 300 || stat.RemainingTraffic != 700 {
		t.Fatalf("deleted usage lost: %+v %v", stat, err)
	}
	if err := service.CheckTrafficQuota(&reseller, 701); err == nil {
		t.Fatal("deleted consumption must constrain future allocations")
	}
	var count int64
	if err := db.Model(&model.ResellerTrafficArchive{}).Where("reseller_id = ?", reseller.Id).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("duplicate archive: count=%d err=%v", count, err)
	}
}

func TestResellerArchiveMonotonicAndSurvivesRecreatedEmail(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	reseller := model.Reseller{Username: "archive-reuse", Password: "hash", Enable: true, TrafficLimit: 1000}
	if err := db.Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	client := model.ClientRecord{Email: "reused@example.test", TotalGB: 100, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}).Error; err != nil {
		t.Fatal(err)
	}
	traffic := xray.ClientTraffic{Email: client.Email, Up: 20, Down: 40}
	if err := db.Create(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if err := archiveResellerClientUsage(&client); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Updates(map[string]any{"up": 1, "down": 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := archiveResellerClientUsage(&client); err != nil {
		t.Fatal(err)
	}
	stat, err := (&ResellerService{}).Stat(&reseller)
	if err != nil || stat.UsedTraffic != 60 || stat.AllocatedTraffic != 100 {
		t.Fatalf("archive shrank or was double-counted: %+v %v", stat, err)
	}
	if _, err := (&ClientService{}).Delete(&InboundService{}, client.Id, false); err != nil {
		t.Fatal(err)
	}
	newClient := model.ClientRecord{Email: client.Email, TotalGB: 200, Enable: true}
	if err := db.Create(&newClient).Error; err != nil {
		t.Fatal(err)
	}
	if newClient.Id == client.Id {
		t.Fatal("expected distinct client identity")
	}
	if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: newClient.Email}).Error; err != nil {
		t.Fatal(err)
	}
	stat, err = (&ResellerService{}).Stat(&reseller)
	if err != nil || stat.UsedTraffic != 60 || stat.AllocatedTraffic != 260 {
		t.Fatalf("recreated client reused prior consumption: %+v %v", stat, err)
	}
}

func TestScopedBulkDeleteKeepsConsumedTraffic(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "bulk-archive", Password: "hash", Enable: true, TrafficLimit: 1000}
	if err := db.Create(&r).Error; err != nil {
		t.Fatal(err)
	}
	inbound := model.Inbound{Tag: "bulk-archive-inbound", Protocol: model.VLESS, Settings: `{"clients":[]}`}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: r.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	var emails []string
	for _, email := range []string{"archive-a@example.test", "archive-b@example.test"} {
		client := model.ClientRecord{Email: email, TotalGB: 200, Enable: true}
		if err := db.Create(&client).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.ResellerClient{ResellerId: r.Id, Email: email}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: inbound.Id}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&xray.ClientTraffic{Email: email, Up: 30, Down: 70}).Error; err != nil {
			t.Fatal(err)
		}
		emails = append(emails, email)
	}
	// Simulate a missing inbound after a legacy migration; the scoped deletion
	// removes its orphaned associations and still needs to preserve usage.
	if err := db.Delete(&model.Inbound{}, inbound.Id).Error; err != nil {
		t.Fatal(err)
	}
	result, _, err := (&ClientService{}).BulkDeleteForReseller(&InboundService{}, r.Id, emails, false, []int{inbound.Id})
	if err != nil || result.Deleted != 2 {
		t.Fatalf("bulk deletion: %+v %v", result, err)
	}
	stat, err := (&ResellerService{}).Stat(&r)
	if err != nil || stat.AllocatedTraffic != 200 || stat.UsedTraffic != 200 || stat.RemainingTraffic != 800 {
		t.Fatalf("bulk consumption lost: %+v %v", stat, err)
	}
}
