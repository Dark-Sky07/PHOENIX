package service

import (
	"encoding/json"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
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
