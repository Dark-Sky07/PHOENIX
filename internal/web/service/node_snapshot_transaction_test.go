package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestNodeSnapshotRollsBackTrafficAndInboundWhenBaselineWriteFails(t *testing.T) {
	db := initTrafficTestDB(t)
	createNodeInbound(t, db, 1, "atomic-node", 41030)
	svc := &InboundService{}
	syncNode(t, svc, 1, "atomic-node", xray.ClientTraffic{Email: "atomic@example.test", Up: 100, Down: 20, Enable: true})
	if err := db.Exec("CREATE TRIGGER reject_node_baseline BEFORE UPDATE ON node_client_traffics BEGIN SELECT RAISE(ABORT, 'injected baseline failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	snap := &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{{Tag: "atomic-node", Remark: "must roll back", ClientStats: []xray.ClientTraffic{{Email: "atomic@example.test", Up: 150, Down: 30, Enable: true}}}}}
	if _, err := svc.setRemoteTrafficLocked(1, snap, false); err == nil {
		t.Fatal("baseline write failure accepted")
	}
	var inbound model.Inbound
	if err := db.Where("tag = ?", "atomic-node").First(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if inbound.Remark == "must roll back" {
		t.Fatal("inbound update committed despite failure")
	}
	traffic := readTraffic(t, db, "atomic@example.test")
	if traffic.Up != 100 || traffic.Down != 20 {
		t.Fatalf("traffic committed despite failure: %+v", traffic)
	}
	var baseline model.NodeClientTraffic
	if err := db.Where("node_id = ? AND email = ?", 1, "atomic@example.test").First(&baseline).Error; err != nil {
		t.Fatal(err)
	}
	if baseline.Up != 100 || baseline.Down != 20 {
		t.Fatalf("baseline changed: %+v", baseline)
	}
	if err := db.Exec("DROP TRIGGER reject_node_baseline").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.setRemoteTrafficLocked(1, snap, false); err != nil {
		t.Fatal(err)
	}
	traffic = readTraffic(t, db, "atomic@example.test")
	if traffic.Up != 150 || traffic.Down != 30 {
		t.Fatalf("retry lost or duplicated usage: %+v", traffic)
	}
}
