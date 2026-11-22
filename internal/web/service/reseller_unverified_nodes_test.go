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
