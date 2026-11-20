package service

import (
	"encoding/json"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestNodeSnapshotCannotRemoveOrChangeResellerAllocation(t *testing.T) {
	for _, test := range []string{"missing inbound", "missing client", "missing traffic", "changed settings quota", "changed traffic quota", "matching"} {
		t.Run(test, func(t *testing.T) {
			reseller, client := seedResellerRenewal(t)
			db := database.GetDB()
			var link model.ClientInbound
			if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
				t.Fatal(err)
			}
			nodeID := 77
			if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("node_id", nodeID).Error; err != nil {
				t.Fatal(err)
			}
			var ib model.Inbound
			if err := db.First(&ib, link.InboundId).Error; err != nil {
				t.Fatal(err)
			}
			remote := &model.Inbound{Tag: ib.Tag, Protocol: ib.Protocol, Settings: ib.Settings, ClientStats: []xray.ClientTraffic{{Email: client.Email, Total: 500, Up: 120, Down: 180, Enable: true}}}
			snap := &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{remote}}
			switch test {
			case "missing inbound":
				snap.Inbounds = nil
			case "missing client":
				remote.Settings = `{"clients":[]}`
			case "missing traffic":
				remote.ClientStats = nil
			case "changed settings quota":
				payload, err := json.Marshal(map[string]any{"clients": []model.Client{{Email: client.Email, ID: client.UUID, TotalGB: 10, Enable: true}}})
				if err != nil {
					t.Fatal(err)
				}
				remote.Settings = string(payload)
			case "changed traffic quota":
				remote.ClientStats[0].Total = 10
			}
			_, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, snap, false)
			if test == "matching" && err != nil {
				t.Fatalf("matching snapshot rejected: %v", err)
			}
			if test != "matching" && err == nil {
				t.Fatal("unsafe snapshot accepted")
			}
			var got model.ClientRecord
			if err := db.First(&got, client.Id).Error; err != nil {
				t.Fatal(err)
			}
			if got.TotalGB != 500 {
				t.Fatalf("quota changed: %+v", got)
			}
			var links, mappings int64
			if err := db.Model(&model.ClientInbound{}).Where("client_id = ? AND inbound_id = ?", client.Id, ib.Id).Count(&links).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ? AND email = ?", reseller.Id, client.Email).Count(&mappings).Error; err != nil {
				t.Fatal(err)
			}
			if links != 1 || mappings != 1 {
				t.Fatalf("ownership lost: links=%d mappings=%d", links, mappings)
			}
			var traffic xray.ClientTraffic
			if err := db.Where("email = ?", client.Email).First(&traffic).Error; err != nil {
				t.Fatal(err)
			}
			if traffic.Up != 120 || traffic.Down != 180 {
				t.Fatalf("traffic changed: %+v", traffic)
			}
		})
	}
}
