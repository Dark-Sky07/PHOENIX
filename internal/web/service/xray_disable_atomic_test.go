package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

func TestXrayBatchDisableFailureRollsBackTrafficAndClientState(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	clientSvc, inboundSvc := &ClientService{}, &InboundService{}
	ib := mkInbound(t, 29221, model.VLESS, `{"clients":[]}`)
	payload := &ClientCreatePayload{Client: model.Client{Email: "exhausted@example.test", TotalGB: 100, Enable: true}, InboundIds: []int{ib.Id}}
	if _, err := clientSvc.Create(inboundSvc, payload); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_disable_record BEFORE UPDATE OF enable ON clients WHEN NEW.enable = 0 BEGIN SELECT RAISE(ABORT, 'injected disable failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	delta := []*xray.ClientTraffic{{Email: payload.Client.Email, Up: 100}}
	restart, _, err := inboundSvc.AddXrayTrafficBatch("disable-failure", nil, delta)
	if err == nil || !restart {
		t.Fatalf("disable failure must request reconcile and fail batch: restart=%v err=%v", restart, err)
	}
	var traffic xray.ClientTraffic
	if err := db.Where("email = ?", payload.Client.Email).First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if traffic.Up != 0 || !traffic.Enable {
		t.Fatalf("failed batch committed traffic or disable: %+v", traffic)
	}
	var saved model.Inbound
	if err := db.First(&saved, ib.Id).Error; err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Clients []struct {
			Enable bool `json:"enable"`
		} `json:"clients"`
	}
	if err := json.Unmarshal([]byte(saved.Settings), &settings); err != nil || len(settings.Clients) != 1 || !settings.Clients[0].Enable {
		t.Fatalf("failed batch left inbound disabled: %s, %v", saved.Settings, err)
	}
	var count int64
	if err := db.Model(&model.XrayTrafficReceipt{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("receipt survived rollback: %d %v", count, err)
	}
	if err := db.Exec("DROP TRIGGER fail_disable_record").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := inboundSvc.AddXrayTrafficBatch("disable-failure", nil, delta); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("email = ?", payload.Client.Email).First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if traffic.Up != 100 || traffic.Enable {
		t.Fatalf("retry failed to account and disable: %+v", traffic)
	}
	var record model.ClientRecord
	if err := db.Where("email = ?", payload.Client.Email).First(&record).Error; err != nil || record.Enable {
		t.Fatalf("record not disabled: %+v %v", record, err)
	}
}

func TestDisableClientPreservesLargeSettingsNumbers(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	ib := mkInbound(t, 29222, model.VLESS, `{"marker":9007199254740993,"clients":[{"email":"large@example.test","enable":true,"totalGB":9007199254740993}]}`)
	_, after, err := (&InboundService{}).markClientsDisabledInSettings(db, ib.Id, map[string]struct{}{"large@example.test": {}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Settings, `9007199254740993`) || !strings.Contains(after.Settings, `"enable": false`) {
		t.Fatalf("settings corrupted: %s", after.Settings)
	}
}

func TestDisableClientRejectsMissingSettingsIdentity(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	ib := mkInbound(t, 29223, model.VLESS, `{"clients":[{"email":"other@example.test","enable":true}]}`)
	if _, _, err := (&InboundService{}).markClientsDisabledInSettings(db, ib.Id, map[string]struct{}{"missing@example.test": {}}); err == nil {
		t.Fatal("missing client should require reconciliation")
	}
}

func TestDisableClientsRequestsRestartWhenXrayAPICannotInitialize(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	previous := p
	p = xray.NewProcess(&xray.Config{}) // process has no API port
	t.Cleanup(func() { p = previous })
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Update("reset", 0).Error; err != nil {
		t.Fatal(err)
	}
	var restart bool
	var count int64
	err := db.Transaction(func(tx *gorm.DB) error {
		var err error
		restart, count, _, err = (&InboundService{}).disableInvalidClients(tx)
		return err
	})
	if err != nil || !restart || count != 1 {
		t.Fatalf("restart=%v count=%d err=%v", restart, count, err)
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Enable {
		t.Fatal("traffic row remains enabled")
	}
}

func TestDisableInboundsRequestsRestartWhenXrayAPICannotInitialize(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	ib := mkInbound(t, 29224, model.VLESS, `{"clients":[]}`)
	if err := db.Model(&model.Inbound{}).Where("id = ?", ib.Id).Updates(map[string]any{"enable": true, "total": 100, "up": 100}).Error; err != nil {
		t.Fatal(err)
	}
	previous := p
	p = xray.NewProcess(&xray.Config{})
	t.Cleanup(func() { p = previous })
	restart, count, err := (&InboundService{}).disableInvalidInbounds(db)
	if err != nil || !restart || count != 1 {
		t.Fatalf("restart=%v count=%d err=%v", restart, count, err)
	}
}
