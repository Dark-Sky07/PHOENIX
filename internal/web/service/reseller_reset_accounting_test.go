package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestResellerUsageSurvivesSupportedTrafficResets(t *testing.T) {
	resetters := map[string]func(string, int) error{
		"single-no-inbound": func(email string, _ int) error { return (&InboundService{}).ResetClientTrafficByEmail(email) },
		"single-inbound": func(email string, id int) error {
			_, err := (&InboundService{}).ResetClientTraffic(id, email)
			return err
		},
		"bulk": func(email string, _ int) error {
			_, err := (&ClientService{}).BulkResetTraffic(&InboundService{}, []string{email, email})
			return err
		},
		"inbound-all": func(_ string, id int) error { return (&ClientService{}).ResetAllClientTraffics(&InboundService{}, id) },
		"global-all":  func(_ string, _ int) error { _, err := (&ClientService{}).ResetAllTraffics(); return err },
	}
	for name, reset := range resetters {
		t.Run(name, func(t *testing.T) {
			r, client := seedResellerRenewal(t)
			db := database.GetDB()
			var link model.ClientInbound
			if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "master", Email: client.Email, Up: 200, Down: 220}).Error; err != nil {
				t.Fatal(err)
			}
			if err := reset(client.Email, link.InboundId); err != nil {
				t.Fatal(err)
			}
			if err := reset(client.Email, link.InboundId); err != nil {
				t.Fatal(err)
			} // idempotent
			stat, err := (&ResellerService{}).Stat(&r)
			if err != nil || stat.UsedTraffic != 420 || stat.AllocatedTraffic != 920 {
				t.Fatalf("first window lost or duplicated: %+v %v", stat, err)
			}
			if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Updates(map[string]any{"up": 40, "down": 60}).Error; err != nil {
				t.Fatal(err)
			}
			if err := reset(client.Email, link.InboundId); err != nil {
				t.Fatal(err)
			}
			stat, err = (&ResellerService{}).Stat(&r)
			if err != nil || stat.UsedTraffic != 520 || stat.AllocatedTraffic != 1020 {
				t.Fatalf("second window lost: %+v %v", stat, err)
			}
		})
	}
}

func TestResellerResetArchiveFailureDoesNotClearTraffic(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_reset_archive BEFORE INSERT ON reseller_traffic_archives BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).ResetClientTrafficByEmail(client.Email); err == nil {
		t.Fatal("expected archive failure")
	}
	var traffic xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if traffic.Up != 120 || traffic.Down != 180 {
		t.Fatalf("failed reset erased traffic: %+v", traffic)
	}
}
