package service

import (
	"encoding/json"
	"errors"
	"strings"
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

func TestAutoRenewalReenablesEveryInboundForSharedClient(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	var first model.Inbound
	if err := db.First(&first, link.InboundId).Error; err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"clients": []model.Client{{Email: client.Email, ID: client.UUID, TotalGB: 500, Enable: false, Reset: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&first).Update("settings", string(payload)).Error; err != nil {
		t.Fatal(err)
	}
	second := model.Inbound{UserId: 1, Port: 19006, Protocol: model.VLESS, Settings: string(payload), Tag: "renew-second"}
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: second.Id}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Update("enable", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, count, err := (&InboundService{}).autoRenewClients(tx)
		if err == nil && count != 1 {
			return errors.New("expected one renewal")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{first.Id, second.Id} {
		var inbound model.Inbound
		if err := db.First(&inbound, id).Error; err != nil {
			t.Fatal(err)
		}
		var settings struct {
			Clients []model.Client `json:"clients"`
		}
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
			t.Fatal(err)
		}
		if len(settings.Clients) != 1 || !settings.Clients[0].Enable || settings.Clients[0].ExpiryTime <= time.Now().UnixMilli() {
			t.Fatalf("inbound %d not reenabled: %+v", id, settings.Clients)
		}
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if !row.Enable || row.Up != 0 || row.Down != 0 {
		t.Fatalf("traffic not renewed: %+v", row)
	}
}

func TestRenewalRejectsStaleResellerInboundQuota(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	settings := `{"clients":[{"email":"renew@example.test","id":"renew-id","totalGB":9007199254740993,"enable":true,"reset":1}]}`
	if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("settings", settings).Error; err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error { _, _, err := (&InboundService{}).autoRenewClients(tx); return err })
	if err == nil {
		t.Fatal("mismatched runtime quota must block renewal")
	}
	var updated model.ClientRecord
	if err := db.First(&updated, client.Id).Error; err != nil {
		t.Fatal(err)
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if updated.TotalGB != 500 || row.Up != 120 || row.Down != 180 {
		t.Fatalf("partial renewal: client=%+v traffic=%+v", updated, row)
	}
}

func TestRenewalPreservesLargeNumbersInSettings(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	settings := `{"marker":9007199254740993,"clients":[{"email":"renew@example.test","id":"renew-id","totalGB":500,"enable":true,"reset":1}]}`
	if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("settings", settings).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error { _, _, err := (&InboundService{}).autoRenewClients(tx); return err }); err != nil {
		t.Fatal(err)
	}
	var inbound model.Inbound
	if err := db.First(&inbound, link.InboundId).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inbound.Settings, `9007199254740993`) {
		t.Fatalf("large integer rounded: %s", inbound.Settings)
	}
}

func TestRenewalRejectsMalformedTypedClientWithoutReset(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	settings := `{"clients":[{"email":"renew@example.test","totalGB":"invalid"}]}`
	if err := db.Model(&model.Inbound{}).Where("id = ?", link.InboundId).Update("settings", settings).Error; err != nil {
		t.Fatal(err)
	}
	err := db.Transaction(func(tx *gorm.DB) error { _, _, err := (&InboundService{}).autoRenewClients(tx); return err })
	if err == nil {
		t.Fatal("expected invalid typed client error")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 120 || row.Down != 180 {
		t.Fatalf("traffic reset: %+v", row)
	}
}

func TestCheckedXrayBatchRenewsWhileCallerHoldsAllocationLock(t *testing.T) {
	r, client := seedResellerRenewal(t)
	svc := &InboundService{}
	unlock, err := LockResellerAllocation()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	identities, err := svc.CaptureXrayTrafficIdentities(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddCheckedXrayTrafficBatchUnderAllocationLock("test-locked-renewal", nil, nil, identities); err != nil {
		t.Fatal(err)
	}
	var row xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 0 || row.Down != 0 || row.ExpiryTime <= time.Now().UnixMilli() {
		t.Fatalf("renewal did not run under held allocation lock: %+v", row)
	}
	stat, err := (&ResellerService{}).Stat(&r)
	if err != nil || stat.UsedTraffic != 300 || stat.AllocatedTraffic != 800 {
		t.Fatalf("renewal usage not preserved: %+v %v", stat, err)
	}
}
