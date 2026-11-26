package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestNegativeTrafficEvidenceBlocksRewritesAndStatDeletion(t *testing.T) {
	for _, operation := range []string{"rewrite", "single-stat", "bulk-stat", "delete-by-email", "delete-id", "delete-scoped", "delete-bulk", "delete-inbound-client"} {
		for _, kind := range []string{"local", "global"} {
			t.Run(operation+"/"+kind, func(t *testing.T) {
				_, client := seedResellerRenewal(t)
				db := database.GetDB()
				if err := db.Where("email = ?", client.Email).Delete(&model.ResellerClient{}).Error; err != nil {
					t.Fatal(err)
				}
				if kind == "local" {
					if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Update("up", -1).Error; err != nil {
						t.Fatal(err)
					}
				} else if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "corrupt", Email: client.Email, Down: -1}).Error; err != nil {
					t.Fatal(err)
				}
				var link model.ClientInbound
				if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
					t.Fatal(err)
				}
				var inbound model.Inbound
				if err := db.First(&inbound, link.InboundId).Error; err != nil {
					t.Fatal(err)
				}
				svc := &InboundService{}
				var err error
				switch operation {
				case "rewrite":
					err = svc.updateClientTrafficByEmail(client.Email, 0, 0)
				case "single-stat":
					err = svc.DelClientStat(db, client.Email)
				case "bulk-stat":
					err = svc.delClientStatsByEmails(db, []string{client.Email})
				case "delete-by-email":
					_, err = (&ClientService{}).DeleteByEmail(svc, client.Email, false)
				case "delete-id":
					_, err = (&ClientService{}).Delete(svc, client.Id, false)
				case "delete-scoped":
					_, err = (&ClientService{}).DeleteByEmailForInbounds(svc, client.Email, false, []int{link.InboundId})
				case "delete-bulk":
					_, _, err = (&ClientService{}).BulkDelete(svc, []string{client.Email}, false)
				case "delete-inbound-client":
					_, err = (&ClientService{}).DelInboundClientByEmail(svc, link.InboundId, client.Email, false)
				}
				if err == nil {
					t.Fatal("mutation accepted corrupt traffic")
				}
				var after model.Inbound
				if err := db.First(&after, link.InboundId).Error; err != nil || after.Settings != inbound.Settings {
					t.Fatalf("inbound settings changed before rejection: %+v %v", after, err)
				}
				var remaining int64
				if err := db.Model(&model.ClientInbound{}).Where("client_id = ? AND inbound_id = ?", client.Id, link.InboundId).Count(&remaining).Error; err != nil || remaining != 1 {
					t.Fatalf("client link removed before rejection: %d %v", remaining, err)
				}
				var row xray.ClientTraffic
				if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil || row.Down != 180 {
					t.Fatalf("traffic evidence lost: %+v %v", row, err)
				}
				if kind == "local" && row.Up != -1 {
					t.Fatalf("local evidence changed: %+v", row)
				}
				if kind == "global" {
					var global model.ClientGlobalTraffic
					if err := db.Where("email = ?", client.Email).Take(&global).Error; err != nil || global.Down != -1 {
						t.Fatalf("global evidence lost: %+v %v", global, err)
					}
				}
			})
		}
	}
}
