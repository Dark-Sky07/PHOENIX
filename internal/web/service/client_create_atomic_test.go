package service

import (
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"testing"
)

func TestClientCreateFailsAndRollsBackWhenTrafficInsertFails(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	in := model.Inbound{UserId: 1, Port: 19418, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "failed-add"}
	if err := db.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_traffic_add BEFORE INSERT ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	payload := ClientCreatePayload{Client: model.Client{Email: "failed-create@example.test", TotalGB: 50, Enable: true}, InboundIds: []int{in.Id}}
	_, err := (&ClientService{}).Create(&InboundService{}, &payload)
	if err == nil {
		t.Fatal("expected traffic row insert failure")
	}
	var count int64
	if err := db.Model(&xray.ClientTraffic{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("traffic count %d, %v", count, err)
	}
	if err := db.Model(&model.ClientRecord{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("client count %d, %v", count, err)
	}
	var saved model.Inbound
	if err := db.First(&saved, in.Id).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Settings != in.Settings {
		t.Fatalf("inbound changed after rollback: %s", saved.Settings)
	}
}

func TestPartialClientCreationCanBeCompensatedByGeneratedIdentity(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	in := model.Inbound{UserId: 1, Port: 19419, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "partial-add"}
	if err := db.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ClientService{}
	payload := ClientCreatePayload{Client: model.Client{Email: "partial@example.test", TotalGB: 50, Enable: true}, InboundIds: []int{in.Id, 999999}}
	_, err := svc.Create(&InboundService{}, &payload)
	if err == nil {
		t.Fatal("expected second inbound to fail")
	}
	if payload.Client.SubID == "" {
		t.Fatal("generated identity was not returned to caller")
	}
	if _, err := svc.GetRecordByEmail(nil, payload.Client.Email); err != nil {
		t.Fatalf("first inbound did not create partial client: %v", err)
	}
	if _, err := svc.RollbackNewClientCreate(&InboundService{}, payload.Client.Email, "different-id"); err == nil {
		t.Fatal("rollback accepted unrelated identity")
	}
	if _, err := svc.RollbackNewClientCreate(&InboundService{}, payload.Client.Email, payload.Client.SubID); err != nil {
		t.Fatalf("compensation: %v", err)
	}
	var count int64
	if err := db.Model(&model.ClientRecord{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial client remains: %d %v", count, err)
	}
	if err := db.Model(&xray.ClientTraffic{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("partial traffic remains: %d %v", count, err)
	}
}
