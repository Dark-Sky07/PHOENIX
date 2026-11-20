package service

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

func seedResellerRenewal(t *testing.T) (model.Reseller, model.ClientRecord) {
	t.Helper()
	setupConflictDB(t)
	db := database.GetDB()
	r := model.Reseller{Username: "renew-test", Password: "hash", Enable: true, TrafficLimit: 2000}
	client := model.ClientRecord{Email: "renew@example.test", UUID: "renew-id", TotalGB: 500, Enable: true, Reset: 1}
	payload, err := json.Marshal(map[string]any{"clients": []model.Client{{Email: client.Email, ID: client.UUID, TotalGB: 500, Enable: true, Reset: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	in := model.Inbound{UserId: 1, Port: 19005, Protocol: model.VLESS, Settings: string(payload), Tag: "renew-inbound"}
	for _, row := range []any{&r, &client, &in} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.ResellerClient{ResellerId: r.Id, Email: client.Email}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: in.Id}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{InboundId: in.Id, Email: client.Email, Enable: true, Up: 120, Down: 180, Total: 500, Reset: 1, ExpiryTime: time.Now().Add(-time.Hour).UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	return r, client
}

func TestResellerAutoRenewalPreservesMultipleWindows(t *testing.T) {
	r, client := seedResellerRenewal(t)
	db := database.GetDB()
	renew := func() {
		t.Helper()
		err := db.Transaction(func(tx *gorm.DB) error {
			_, count, err := (&InboundService{}).autoRenewClients(tx)
			if err != nil {
				return err
			}
			if count != 1 {
				return errors.New("expected one automatic renewal")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("renewal: %v", err)
		}
	}
	renew()
	stat, err := (&ResellerService{}).Stat(&r)
	if err != nil || stat.UsedTraffic != 300 || stat.AllocatedTraffic != 800 {
		t.Fatalf("first renewal: %+v %v", stat, err)
	}
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Updates(map[string]any{"up": 40, "down": 60, "expiry_time": time.Now().Add(-time.Hour).UnixMilli()}).Error; err != nil {
		t.Fatal(err)
	}
	renew()
	stat, err = (&ResellerService{}).Stat(&r)
	if err != nil || stat.UsedTraffic != 400 || stat.AllocatedTraffic != 900 {
		t.Fatalf("second renewal: %+v %v", stat, err)
	}
	if _, err := (&ClientService{}).Delete(&InboundService{}, client.Id, false); err != nil {
		t.Fatal(err)
	}
	stat, err = (&ResellerService{}).Stat(&r)
	if err != nil || stat.UsedTraffic != 400 || stat.AllocatedTraffic != 400 {
		t.Fatalf("deletion after renewal: %+v %v", stat, err)
	}
}

func TestRenewalArchiveFailureRollsBackTransaction(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_renewal_archive BEFORE INSERT ON reseller_traffic_archives BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error { _, _, err := (&InboundService{}).autoRenewClients(tx); return err })
	if err == nil {
		t.Fatal("expected archive failure")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 120 || row.Down != 180 || row.ExpiryTime > time.Now().UnixMilli() {
		t.Fatalf("failed renewal changed traffic: %+v", row)
	}
}

func TestRenewalSyncFailureRollsBackArchiveAndAssociations(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_renewal_link BEFORE INSERT ON client_inbounds BEGIN SELECT RAISE(ABORT, 'injected sync failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error { _, _, err := (&InboundService{}).autoRenewClients(tx); return err })
	if err == nil {
		t.Fatal("expected sync failure")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 120 || row.Down != 180 || row.ExpiryTime > time.Now().UnixMilli() {
		t.Fatalf("traffic changed: %+v", row)
	}
	var links, archives int64
	if err := db.Model(&model.ClientInbound{}).Where("client_id = ?", client.Id).Count(&links).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ResellerTrafficArchive{}).Count(&archives).Error; err != nil {
		t.Fatal(err)
	}
	if links != 1 || archives != 0 {
		t.Fatalf("partial renewal committed: links=%d archives=%d", links, archives)
	}
}

func TestRenewalMalformedInboundReturnsError(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("settings", `{"clients":[null]}`).Error; err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error { _, _, err := (&InboundService{}).autoRenewClients(tx); return err })
	if err == nil {
		t.Fatal("expected malformed client error")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 120 || row.Down != 180 {
		t.Fatalf("traffic changed: %+v", row)
	}
}
