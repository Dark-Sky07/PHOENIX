package service

import (
	"encoding/json"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestPrepareVerifiedNodeGlobalTraffic(t *testing.T) {
	db := initTrafficTestDB(t)
	for _, row := range []any{&xray.ClientTraffic{Email: "forward@example.test", Enable: true, Up: 20}, &model.NodeClientTraffic{NodeId: 1, Email: "forward@example.test"}, &model.TrafficSnapshotSender{ID: "node:1", Sender: "master", Generation: "g"}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	token := "0123456789012345678901234567890123456789"
	svc := &InboundService{}
	e, err := svc.PrepareVerifiedNodeGlobalTraffic(1, "master", token)
	if err != nil {
		t.Fatal(err)
	}
	key, err := trafficprotocol.KeyFromAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := trafficprotocol.Verify(*e, key, trafficprotocol.Expected{Sender: "master", Generation: "g"}); err != nil {
		t.Fatal(err)
	}
	var rows []*xray.ClientTraffic
	if err := json.Unmarshal(e.Snapshot, &rows); err != nil || len(rows) != 1 || rows[0].Up != 20 {
		t.Fatalf("snapshot: %+v %v", rows, err)
	}
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", "forward@example.test").Update("up", 30).Error; err != nil {
		t.Fatal(err)
	}
	next, err := svc.PrepareVerifiedNodeGlobalTraffic(1, "master", token)
	if err != nil || next.Sequence != 2 {
		t.Fatalf("next: %+v %v", next, err)
	}
	if err := json.Unmarshal(next.Snapshot, &rows); err != nil || len(rows) != 1 || rows[0].Up != 30 {
		t.Fatalf("next snapshot: %+v %v", rows, err)
	}
	if _, err := svc.PrepareVerifiedNodeGlobalTraffic(1, "other-master", token); err == nil {
		t.Fatal("different panel GUID accepted")
	}
	if _, err := svc.PrepareVerifiedNodeGlobalTraffic(1, "master", "short"); err == nil {
		t.Fatal("short token accepted")
	}
	if err := database.GetDB().Migrator().DropTable(&model.ClientGlobalTraffic{}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PrepareVerifiedNodeGlobalTraffic(1, "master", token); err == nil {
		t.Fatal("incomplete overlay signed")
	}
}
