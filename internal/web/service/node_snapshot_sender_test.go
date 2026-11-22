package service

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestVerifiedNodeSnapshotCaptureAndSequence(t *testing.T) {
	db := initTrafficTestDB(t)
	var user model.User
	if err := db.Order("id asc").First(&user).Error; err != nil {
		t.Fatal(err)
	}
	guid, err := (&SettingService{}).GetPanelGuid()
	if err != nil {
		t.Fatal(err)
	}
	state := model.TrafficSnapshotSender{ID: "node-snapshot:api-token:7", Sender: guid, Generation: "epoch"}
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
	inbound := model.Inbound{UserId: user.Id, Tag: "local-test", Port: 45550, Protocol: model.VLESS, Settings: `{"clients":[{"email":"test@example.test"}]}`}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	token := "0123456789012345678901234567890123456789"
	svc := &InboundService{}
	if _, err := svc.PrepareVerifiedInboundSnapshot(7, user.Id, token); err == nil {
		t.Fatal("missing traffic accepted")
	}
	if err := db.First(&state, "id = ?", state.ID).Error; err != nil || state.LastSequence != 0 {
		t.Fatalf("failed capture reserved sequence: %+v %v", state, err)
	}
	if err := db.Create(&xray.ClientTraffic{InboundId: inbound.Id, Email: "test@example.test", Up: 100, Down: 40}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "other", Email: "test@example.test", Up: 9999}).Error; err != nil {
		t.Fatal(err)
	}
	env, err := svc.PrepareVerifiedInboundSnapshot(7, user.Id, token)
	if err != nil {
		t.Fatal(err)
	}
	key, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := trafficprotocol.Verify(*env, key, trafficprotocol.Expected{Sender: guid, Generation: "epoch"}); err != nil {
		t.Fatal(err)
	}
	var snap runtime.TrafficSnapshot
	if err := json.Unmarshal(env.Snapshot, &snap); err != nil {
		t.Fatal(err)
	}
	if env.Sequence != 1 || len(snap.Inbounds) != 1 || len(snap.Inbounds[0].ClientStats) != 1 || snap.Inbounds[0].ClientStats[0].Up != 100 {
		t.Fatalf("wrong snapshot: %+v %+v", env, snap)
	}
	next, err := svc.PrepareVerifiedInboundSnapshot(7, user.Id, token)
	if err != nil || next.Sequence != 2 {
		t.Fatalf("next sequence: %+v %v", next, err)
	}
	if _, err := svc.PrepareVerifiedInboundSnapshot(8, user.Id, token); err == nil {
		t.Fatal("unprovisioned token accepted")
	}
	nodeID := 1
	other := model.Inbound{UserId: user.Id, Tag: "nested-test", Port: 45551, Protocol: model.VLESS, NodeID: &nodeID, Settings: `{"clients":[]}`}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrepareVerifiedInboundSnapshot(7, user.Id, token); err == nil {
		t.Fatal("nested nodes silently omitted")
	}
	if err := db.First(&state, "id = ?", fmt.Sprintf("node-snapshot:api-token:%d", 7)).Error; err != nil || state.LastSequence != 2 {
		t.Fatalf("invalid snapshot advanced sequence: %+v %v", state, err)
	}
}
