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

func TestVerifiedNodeSnapshotSequenceAndAccountingRollback(t *testing.T) {
	db := initTrafficTestDB(t)
	node := model.Node{Name: "verified-inbound", Address: "example.com", Port: 443, Scheme: "https", ApiToken: "0123456789012345678901234567890123456789"}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.TrafficSnapshotReceiver{ID: fmt.Sprintf("node-inbound:%d", node.Id), Sender: "node-guid", Generation: "epoch"}).Error; err != nil {
		t.Fatal(err)
	}
	createNodeInbound(t, db, node.Id, "verified-tag", 41234)
	key, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(node.ApiToken)
	if err != nil {
		t.Fatal(err)
	}
	makeSnapshot := func(seq uint64, up int64) trafficprotocol.Envelope {
		snap := runtime.TrafficSnapshot{Inbounds: []*model.Inbound{{Tag: "verified-tag", Settings: `{"clients":[]}`, ClientStats: []xray.ClientTraffic{{Email: "verified-node@example.test", Up: up, Enable: true}}}}}
		b, e := json.Marshal(snap)
		if e != nil {
			t.Fatal(e)
		}
		env, e := trafficprotocol.Sign(trafficprotocol.Envelope{Version: trafficprotocol.Version, Sender: "node-guid", Generation: "epoch", Sequence: seq, Snapshot: b}, key)
		if e != nil {
			t.Fatal(e)
		}
		return env
	}
	svc := &InboundService{}
	first := makeSnapshot(1, 100)
	if _, err := svc.acceptVerifiedNodeTrafficLocked(node.Id, first); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.acceptVerifiedNodeTrafficLocked(node.Id, first); err == nil {
		t.Fatal("replay accepted")
	}
	if err := db.Exec("CREATE TRIGGER reject_verified_node_baseline BEFORE UPDATE ON node_client_traffics BEGIN SELECT RAISE(ABORT, 'injected'); END").Error; err != nil {
		t.Fatal(err)
	}
	next := makeSnapshot(2, 150)
	if _, err := svc.acceptVerifiedNodeTrafficLocked(node.Id, next); err == nil {
		t.Fatal("failed accounting accepted")
	}
	var state model.TrafficSnapshotReceiver
	if err := db.First(&state, "id = ?", fmt.Sprintf("node-inbound:%d", node.Id)).Error; err != nil || state.LastSequence != 1 {
		t.Fatalf("sequence not rolled back: %+v %v", state, err)
	}
	if row := readTraffic(t, db, "verified-node@example.test"); row.Up != 100 {
		t.Fatalf("accounting not rolled back: %+v", row)
	}
	if err := db.Exec("DROP TRIGGER reject_verified_node_baseline").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := svc.acceptVerifiedNodeTrafficLocked(node.Id, next); err != nil {
		t.Fatal(err)
	}
	if row := readTraffic(t, db, "verified-node@example.test"); row.Up != 150 {
		t.Fatalf("retry failed: %+v", row)
	}
	bad := makeSnapshot(3, 200)
	bad.MAC[0] ^= 1
	if _, err := svc.acceptVerifiedNodeTrafficLocked(node.Id, bad); err == nil {
		t.Fatal("invalid MAC accepted")
	}
}
