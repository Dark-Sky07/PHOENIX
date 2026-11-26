package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
)

func TestResellerAllocationBlockedByUnknownNodeBaseline(t *testing.T) {
	for _, kind := range []string{"missing", "negative", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
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
			if kind != "missing" {
				up := int64(120)
				if kind == "negative" {
					up = -1
				}
				if err := db.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: client.Email, Up: up, Down: 180}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if kind == "duplicate" {
				if err := db.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: "RENEW@example.test", Up: 120, Down: 180}).Error; err != nil {
					t.Fatal(err)
				}
			}
			svc := &ResellerService{}
			if _, err := svc.Stat(&reseller); err == nil {
				t.Fatal("uncertain usage shown as complete")
			}
			if err := svc.CheckTrafficQuota(&reseller, 1); err == nil {
				t.Fatal("finite reseller allocation allowed")
			}
			unlimited := reseller
			unlimited.TrafficLimit = 0
			if err := svc.CheckTrafficQuota(&unlimited, 1); err == nil {
				t.Fatal("unlimited reseller bypassed reconciliation")
			}
			err := db.Transaction(func(tx *gorm.DB) error {
				tracker := renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}
				_, err := tracker.reserve(client.Email)
				return err
			})
			if err == nil {
				t.Fatal("renewal allowed with uncertain baseline")
			}
		})
	}
}

func TestRestoredNodeBaselineStillRequiresVerifiedRemoteSnapshots(t *testing.T) {
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
	// In production the values must be restored from trustworthy evidence.
	if err := db.Create(&model.NodeClientTraffic{NodeId: nodeID, Email: client.Email, Up: 120, Down: 180}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ResellerService{}
	stat, err := svc.Stat(&reseller)
	if err != nil || stat.UsedTraffic != 300 || stat.AllocatedTraffic != 500 {
		t.Fatalf("stat=%+v err=%v", stat, err)
	}
	if err := svc.CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("restored baseline must not authenticate remote snapshots")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		tracker := renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}
		_, err := tracker.reserve(client.Email)
		return err
	}); err == nil {
		t.Fatal("renewal accepted without authenticated remote snapshots")
	}
}
