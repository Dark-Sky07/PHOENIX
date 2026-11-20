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
	for _, test := range []string{"missing inbound", "missing client", "missing traffic", "changed settings quota", "changed traffic quota", "changed credentials", "changed expiry", "changed traffic expiry", "matching"} {
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
			var existing xray.ClientTraffic
			if err := db.Where("email = ?", client.Email).First(&existing).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: client.Email, Up: existing.Up, Down: existing.Down}).Error; err != nil {
				t.Fatal(err)
			}
			remote := &model.Inbound{Tag: ib.Tag, Protocol: ib.Protocol, Settings: ib.Settings, ClientStats: []xray.ClientTraffic{{Email: client.Email, Total: 500, Up: 120, Down: 180, Enable: true, ExpiryTime: existing.ExpiryTime, Reset: existing.Reset}}}
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
			case "changed credentials", "changed expiry":
				var settings struct {
					Clients []model.Client `json:"clients"`
				}
				if err := json.Unmarshal([]byte(remote.Settings), &settings); err != nil {
					t.Fatal(err)
				}
				if test == "changed credentials" {
					settings.Clients[0].ID = "altered"
				} else {
					settings.Clients[0].ExpiryTime = 12345
				}
				data, err := json.Marshal(settings)
				if err != nil {
					t.Fatal(err)
				}
				remote.Settings = string(data)
			case "changed traffic expiry":
				remote.ClientStats[0].ExpiryTime = 0
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

func TestNodeSnapshotCannotIntroduceResellerAssociation(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		existing, dirty, statsOnly bool
	}{
		{"new inbound", false, false, false},
		{"existing inbound", true, false, false},
		{"stats only", true, false, true},
		{"dirty snapshot", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reseller, client := seedResellerRenewal(t)
			db := database.GetDB()
			const nodeID = 77
			if tc.existing {
				createNodeInbound(t, db, nodeID, "remote-inbound", 19240)
			}
			settings, err := json.Marshal(map[string]any{"clients": []model.Client{{Email: client.Email, ID: client.UUID, TotalGB: 10, Enable: true}}})
			if err != nil {
				t.Fatal(err)
			}
			remote := &model.Inbound{Tag: "remote-inbound", Protocol: model.VLESS, Settings: string(settings), ClientStats: []xray.ClientTraffic{{Email: client.Email, Up: 1, Total: 10}}}
			if tc.statsOnly {
				remote.Settings = `{"clients":[]}`
			}
			_, err = (&InboundService{}).setRemoteTrafficLocked(nodeID, &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{remote}}, tc.dirty)
			if err == nil {
				t.Fatal("unapproved reseller association accepted")
			}
			var stored model.ClientRecord
			if err := db.First(&stored, client.Id).Error; err != nil {
				t.Fatal(err)
			}
			if stored.TotalGB != 500 {
				t.Fatalf("canonical quota changed: %+v", stored)
			}
			var traffic xray.ClientTraffic
			if err := db.Where("email = ?", client.Email).First(&traffic).Error; err != nil {
				t.Fatal(err)
			}
			if traffic.Up != 120 || traffic.Down != 180 {
				t.Fatalf("client traffic changed: %+v", traffic)
			}
			var links, mappings int64
			if err := db.Model(&model.ClientInbound{}).Where("client_id = ?", client.Id).Count(&links).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ? AND email = ?", reseller.Id, client.Email).Count(&mappings).Error; err != nil {
				t.Fatal(err)
			}
			if links != 1 || mappings != 1 {
				t.Fatalf("unexpected ownership change: links=%d mappings=%d", links, mappings)
			}
		})
	}
}

func TestNodeSyncDoesNotOverwriteCanonicalResellerIdentity(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	const nodeID = 77
	if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("node_id", nodeID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ClientRecord{}).Where("id = ?", client.Id).Updates(map[string]any{"uuid": "canonical-identity", "sub_id": "canonical-sub", "renewal_mode": 1, "renewal_day": 15}).Error; err != nil {
		t.Fatal(err)
	}
	var ib model.Inbound
	var local xray.ClientTraffic
	if err := db.First(&ib, link.InboundId).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("email = ?", client.Email).First(&local).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: client.Email, Up: local.Up, Down: local.Down}).Error; err != nil {
		t.Fatal(err)
	}
	remote := &model.Inbound{Tag: ib.Tag, Protocol: ib.Protocol, Settings: ib.Settings, ClientStats: []xray.ClientTraffic{local}}
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{remote}}, false); err != nil {
		t.Fatal(err)
	}
	var saved model.ClientRecord
	if err := db.First(&saved, client.Id).Error; err != nil {
		t.Fatal(err)
	}
	if saved.UUID != "canonical-identity" || saved.SubID != "canonical-sub" || saved.RenewalMode != 1 || saved.RenewalDay != 15 || saved.TotalGB != 500 {
		t.Fatalf("canonical identity overwritten: %+v", saved)
	}
}

func TestNodeSyncLinkFailureRollsBackTrafficAndBaseline(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	const nodeID = 77
	if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("node_id", nodeID).Error; err != nil {
		t.Fatal(err)
	}
	var ib model.Inbound
	var local xray.ClientTraffic
	if err := db.First(&ib, link.InboundId).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("email = ?", client.Email).First(&local).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: client.Email, Up: local.Up, Down: local.Down}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_node_link BEFORE INSERT ON client_inbounds BEGIN SELECT RAISE(ABORT, 'injected sync failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	remote := &model.Inbound{Tag: ib.Tag, Protocol: ib.Protocol, Settings: ib.Settings, ClientStats: []xray.ClientTraffic{local}}
	if _, err := (&InboundService{}).setRemoteTrafficLocked(nodeID, &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{remote}}, false); err == nil {
		t.Fatal("failed sync was accepted")
	}
	var links, baselines int64
	if err := db.Model(&model.ClientInbound{}).Where("client_id = ?", client.Id).Count(&links).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.NodeClientTraffic{}).Where("node_id = ?", nodeID).Count(&baselines).Error; err != nil {
		t.Fatal(err)
	}
	if links != 1 || baselines != 1 {
		t.Fatalf("partial sync committed: links=%d baselines=%d", links, baselines)
	}
}

func TestNodeResellerMissingBaselineDoesNotDiscardFirstInterval(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	const nodeID = 77
	if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("node_id", nodeID).Error; err != nil {
		t.Fatal(err)
	}
	var ib model.Inbound
	var local xray.ClientTraffic
	if err := db.First(&ib, link.InboundId).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("email = ?", client.Email).First(&local).Error; err != nil {
		t.Fatal(err)
	}
	remoteTraffic := local
	remoteTraffic.Up, remoteTraffic.Down = 150, 200
	snapshot := &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{{Tag: ib.Tag, Protocol: ib.Protocol, Settings: ib.Settings, ClientStats: []xray.ClientTraffic{remoteTraffic}}}}
	svc := &InboundService{}
	if _, err := svc.setRemoteTrafficLocked(nodeID, snapshot, false); err == nil {
		t.Fatal("missing baseline accepted")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 120 || row.Down != 180 {
		t.Fatalf("ambiguous interval changed traffic: %+v", row)
	}
	var count int64
	if err := db.Model(&model.NodeClientTraffic{}).Where("node_id = ?", nodeID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("ambiguous baseline stored: %d %v", count, err)
	}
	// Simulate an administrator restoring a known baseline from evidence.
	if err := db.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: client.Email, Up: 120, Down: 180}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.setRemoteTrafficLocked(nodeID, snapshot, false); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 150 || row.Down != 200 {
		t.Fatalf("known interval not accounted: %+v", row)
	}
}
