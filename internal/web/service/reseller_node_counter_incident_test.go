package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

func TestResellerNodeCounterDecreasePersistsIncident(t *testing.T) {
	for _, direction := range []string{"up", "down"} {
		t.Run(direction, func(t *testing.T) {
			reseller, client := seedResellerRenewal(t)
			db := database.GetDB()
			var link model.ClientInbound
			if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
				t.Fatal(err)
			}
			const nodeID = 77
			if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("node_id", nodeID).Error; err != nil {
				t.Fatal(err)
			}
			var inbound model.Inbound
			if err := db.First(&inbound, link.InboundId).Error; err != nil {
				t.Fatal(err)
			}
			var local xray.ClientTraffic
			if err := db.Where("email = ?", client.Email).First(&local).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: client.Email, Up: 120, Down: 180}).Error; err != nil {
				t.Fatal(err)
			}
			remote := &model.Inbound{Tag: inbound.Tag, Protocol: inbound.Protocol, Settings: inbound.Settings, ClientStats: []xray.ClientTraffic{{Email: client.Email, Total: 500, Up: 120, Down: 180, Enable: true, ExpiryTime: local.ExpiryTime, Reset: local.Reset}}}
			if direction == "up" {
				remote.ClientStats[0].Up = 119
			} else {
				remote.ClientStats[0].Down = 179
			}
			snap := &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{remote}}
			svc := &InboundService{}
			if _, err := svc.SetRemoteTraffic(nodeID, snap, false); err == nil {
				t.Fatal("decreased counter accepted")
			}
			var incident model.ResellerNodeCounterIncident
			if err := db.Where("node_id = ? AND email = ?", nodeID, client.Email).First(&incident).Error; err != nil {
				t.Fatal(err)
			}
			if incident.PreviousUp != 120 || incident.PreviousDown != 180 || incident.ObservedUp != remote.ClientStats[0].Up || incident.ObservedDown != remote.ClientStats[0].Down {
				t.Fatalf("incorrect evidence: %+v", incident)
			}
			var baseline model.NodeClientTraffic
			if err := db.Where("node_id = ? AND email = ?", nodeID, client.Email).First(&baseline).Error; err != nil {
				t.Fatal(err)
			}
			if baseline.Up != 120 || baseline.Down != 180 {
				t.Fatalf("baseline changed: %+v", baseline)
			}
			remote.ClientStats[0].Up, remote.ClientStats[0].Down = 200, 250
			if _, err := svc.SetRemoteTraffic(nodeID, snap, false); err == nil {
				t.Fatal("later counter silently cleared incident")
			}
			if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
				t.Fatal("allocation allowed with incident")
			}
			if err := db.Transaction(func(tx *gorm.DB) error {
				tracker := renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}
				_, err := tracker.reserve(client.Email)
				return err
			}); err == nil {
				t.Fatal("renewal allowed with incident")
			}
		})
	}
}
