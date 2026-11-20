package service

import (
	"strings"
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
			// A detached link must not make the unresolved incident invisible to
			// the owner or let node synchronization silently resume.
			if err := db.Delete(&link).Error; err != nil {
				t.Fatal(err)
			}
			if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
				t.Fatal("detaching hid incident from allocation")
			}
			if _, err := svc.SetRemoteTraffic(nodeID, snap, false); err == nil {
				t.Fatal("detaching hid incident from node sync")
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

func TestResellerIncidentPreventsUnassignmentAndDeletion(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	db := database.GetDB()
	other := model.ClientRecord{Email: "other-unassign@example.test", TotalGB: 20, Enable: true}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: other.Email}).Error; err != nil {
		t.Fatal(err)
	}
	incident := model.ResellerNodeCounterIncident{NodeId: 77, Email: client.Email, PreviousUp: 120, ObservedUp: 1}
	if err := db.Create(&incident).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ResellerService{}
	if err := svc.UnassignClients(reseller.Id, []string{other.Email, "  RENEW@example.test  "}); err == nil {
		t.Fatal("batch unassignment accepted unresolved incident")
	}
	var count int64
	if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ?", reseller.Id).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("batch was not rolled back: count=%d err=%v", count, err)
	}
	if err := svc.Delete(reseller.Id); err == nil {
		t.Fatal("reseller deletion erased incident ownership")
	}
	if err := db.Model(&model.Reseller{}).Where("id = ?", reseller.Id).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("reseller lost: count=%d err=%v", count, err)
	}
	if err := svc.UnassignClient(reseller.Id, other.Email); err != nil {
		t.Fatalf("unrelated client blocked: %v", err)
	}
	// An administrator must first perform evidence-based reconciliation.
	if err := db.Delete(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.UnassignClient(reseller.Id, client.Email); err != nil {
		t.Fatal(err)
	}
}

func TestNodeIncidentBlocksClientRemovalBeforeMutation(t *testing.T) {
	for _, operation := range []string{"single", "email", "scoped", "bulk", "scoped bulk", "detach", "depleted"} {
		t.Run(operation, func(t *testing.T) {
			reseller, client := seedResellerRenewal(t)
			db := database.GetDB()
			var link model.ClientInbound
			if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
				t.Fatal(err)
			}
			incident := model.ResellerNodeCounterIncident{NodeId: 77, Email: client.Email, PreviousUp: 120, ObservedUp: 1}
			if err := db.Create(&incident).Error; err != nil {
				t.Fatal(err)
			}
			clients := &ClientService{}
			inbound := &InboundService{}
			switch operation {
			case "single":
				if _, err := clients.Delete(inbound, client.Id, false); err == nil {
					t.Fatal("deletion allowed")
				}
			case "email":
				if _, err := clients.DeleteByEmail(inbound, client.Email, false); err == nil {
					t.Fatal("email deletion allowed")
				}
			case "scoped":
				if _, err := clients.DeleteByEmailForInbounds(inbound, client.Email, false, []int{link.InboundId}); err == nil {
					t.Fatal("scoped deletion allowed")
				}
			case "bulk":
				if _, _, err := clients.BulkDelete(inbound, []string{client.Email}, false); err == nil {
					t.Fatal("bulk deletion allowed")
				}
			case "scoped bulk":
				res, _, err := clients.BulkDeleteForInbounds(inbound, []string{client.Email}, false, []int{link.InboundId})
				if err != nil || res.Deleted != 0 || len(res.Skipped) != 1 {
					t.Fatalf("scoped bulk = %+v err=%v", res, err)
				}
			case "detach":
				if _, err := clients.Detach(inbound, client.Id, []int{link.InboundId}); err == nil {
					t.Fatal("detach allowed")
				}
			case "depleted":
				if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Update("reset", 0).Error; err != nil {
					t.Fatal(err)
				}
				if _, _, err := clients.DelDepleted(inbound); err == nil {
					t.Fatal("cleanup allowed")
				}
			}
			for _, query := range []struct {
				table any
				where string
				args  []any
			}{
				{&model.ClientRecord{}, "id = ?", []any{client.Id}},
				{&model.ClientInbound{}, "client_id = ? AND inbound_id = ?", []any{client.Id, link.InboundId}},
				{&model.ResellerClient{}, "reseller_id = ? AND email = ?", []any{reseller.Id, client.Email}},
				{&xray.ClientTraffic{}, "email = ?", []any{client.Email}},
			} {
				var n int64
				if err := db.Model(query.table).Where(query.where, query.args...).Count(&n).Error; err != nil || n != 1 {
					t.Fatalf("record changed: %s count=%d err=%v", query.where, n, err)
				}
			}
			var ib model.Inbound
			if err := db.First(&ib, link.InboundId).Error; err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(ib.Settings, client.Email) {
				t.Fatal("inbound mutated before guard")
			}
		})
	}
}
