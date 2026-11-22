package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestResellerRemoteInboundBlocksQuotaEvenWithoutClients(t *testing.T) {
	reseller, _ := seedResellerRenewal(t)
	db := database.GetDB()
	id := 91
	in := model.Inbound{Tag: "remote-quota-test", NodeID: &id, Protocol: model.VLESS}
	if err := db.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: in.Id}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ResellerService{}
	if err := svc.CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("traffic quota accepted remote inbound")
	}
	if err := svc.CheckClientQuota(&reseller, 1, 1); err == nil {
		t.Fatal("client quota accepted remote inbound")
	}
	unlimited := reseller
	unlimited.TrafficLimit = 0
	unlimited.ClientLimit = 0
	if err := svc.CheckClientQuota(&unlimited, 1, 1); err == nil {
		t.Fatal("unlimited quota bypassed remote guard")
	}
	if err := db.Migrator().DropTable(&model.ResellerInbound{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckClientQuota(&reseller, 1, 1); err == nil {
		t.Fatal("database error treated as clear")
	}
}

func TestResellerRemoteGuardRejectsOrphanedEvidence(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := rejectResellerUnverifiedNodes(db, reseller.Id); err != nil {
		t.Fatalf("local-only client blocked: %v", err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: 999991}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectResellerUnverifiedNodes(db, reseller.Id); err == nil {
		t.Fatal("orphaned assignment accepted")
	}
	if err := db.Where("reseller_id = ?", reseller.Id).Delete(&model.ResellerInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: 999992}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectResellerUnverifiedNodes(db, reseller.Id); err == nil {
		t.Fatal("orphaned client link accepted")
	}
	if err := db.Where("client_id = ? AND inbound_id = ?", client.Id, 999992).Delete(&model.ClientInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.NodeClientTraffic{NodeId: 73, Email: "  RENEW@example.test ", Up: 5}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectResellerUnverifiedNodes(db, reseller.Id); err == nil {
		t.Fatal("orphaned remote baseline accepted")
	}
	if err := db.Where("node_id = ?", 73).Delete(&model.NodeClientTraffic{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectResellerUnverifiedNodes(db, reseller.Id); err != nil {
		t.Fatalf("local-only client blocked after removing test evidence: %v", err)
	}
}

func TestClientQuotaRejectsPendingPollAndCoreExitEvenWhenUnlimited(t *testing.T) {
	reseller, _ := seedResellerRenewal(t)
	reseller.ClientLimit, reseller.TrafficLimit = 0, 0
	svc := &ResellerService{}
	if err := svc.CheckClientQuota(&reseller, 1, 1); err != nil {
		t.Fatalf("clean local account rejected: %v", err)
	}
	if err := BeginXrayPoll("client-quota-pending"); err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckClientQuota(&reseller, 1, 1); err == nil {
		t.Fatal("unlimited client quota bypassed pending poll")
	}
	if err := FinishXrayPoll("client-quota-pending"); err != nil {
		t.Fatal(err)
	}
	// Use the DB incident without a local fallback, as if another host had
	// persisted it. No missing usage is guessed.
	if err := database.GetDB().Create(&model.ResellerNodeCounterIncident{NodeId: 0, Direction: "core-exited"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.CheckClientQuota(&reseller, 1, 1); err == nil {
		t.Fatal("unlimited client quota bypassed core-exit incident")
	}
}
