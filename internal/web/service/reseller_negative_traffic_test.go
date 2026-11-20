package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

func TestCorruptNegativeResellerTrafficBlocksAllocationAndRenewal(t *testing.T) {
	for _, kind := range []string{"local", "global", "archive current", "archive prior"} {
		t.Run(kind, func(t *testing.T) {
			reseller, client := seedResellerRenewal(t)
			db := database.GetDB()
			switch kind {
			case "local":
				if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Update("up", -1).Error; err != nil {
					t.Fatal(err)
				}
			case "global":
				if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "corrupt-master", Email: client.Email, Down: -1}).Error; err != nil {
					t.Fatal(err)
				}
			default:
				archive := model.ResellerTrafficArchive{ResellerId: reseller.Id, ClientRecordId: client.Id, Email: client.Email}
				if kind == "archive current" {
					archive.ConsumedBytes = -1
				} else {
					archive.PriorConsumedBytes = -1
				}
				if err := db.Create(&archive).Error; err != nil {
					t.Fatal(err)
				}
			}
			svc := &ResellerService{}
			if _, err := svc.Stat(&reseller); err == nil {
				t.Fatal("corrupt traffic accepted in reseller statistics")
			}
			if err := svc.CheckTrafficQuota(&reseller, 1); err == nil {
				t.Fatal("corrupt traffic freed allocation")
			}
			if err := db.Transaction(func(tx *gorm.DB) error {
				tracker := renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}
				_, err := tracker.reserve(client.Email)
				return err
			}); err == nil {
				t.Fatal("corrupt traffic accepted for renewal quota")
			}
			if err := db.Transaction(func(tx *gorm.DB) error { return archiveResellerClientUsageTx(tx, &client, 0) }); err == nil {
				t.Fatal("corrupt traffic accepted in archive")
			}
			if err := db.Transaction(func(tx *gorm.DB) error { _, _, err := (&InboundService{}).autoRenewClients(tx); return err }); err == nil {
				t.Fatal("corrupt traffic accepted in auto renewal")
			}
			var row xray.ClientTraffic
			if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if row.Down != 180 {
				t.Fatalf("renewal reset traffic: %+v", row)
			}
		})
	}
}

func TestTrafficWriterDoesNotEraseCorruptClientCounter(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Update("up", -1).Error; err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		return (&InboundService{}).addClientTraffic(tx, []*xray.ClientTraffic{{Email: client.Email, Up: 10}})
	})
	if err == nil {
		t.Fatal("corrupt traffic accepted")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != -1 || row.Down != 180 {
		t.Fatalf("counter silently rewritten: %+v", row)
	}
}
