package service

import (
	"math"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestNodeSnapshotRejectsInvalidCountersWithoutAdvancingBaseline(t *testing.T) {
	for _, kind := range []string{"negative client", "duplicate client", "conflicting sibling", "negative baseline", "negative local"} {
		t.Run(kind, func(t *testing.T) {
			db := initTrafficTestDB(t)
			createNodeInbound(t, db, 1, "node-a", 41201)
			svc := &InboundService{}
			syncNode(t, svc, 1, "node-a", xray.ClientTraffic{Email: "alice", Up: 100, Down: 90, Enable: true})
			snap := &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{{Tag: "node-a", ClientStats: []xray.ClientTraffic{{Email: "alice", Up: 120, Down: 100, Enable: true}}}}}
			switch kind {
			case "negative client":
				snap.Inbounds[0].ClientStats[0].Up = -1
			case "duplicate client":
				snap.Inbounds[0].ClientStats = append(snap.Inbounds[0].ClientStats, xray.ClientTraffic{Email: " ALICE ", Up: 5})
			case "conflicting sibling":
				snap.Inbounds = append(snap.Inbounds, &model.Inbound{Tag: "node-b", ClientStats: []xray.ClientTraffic{{Email: "alice", Up: 5}}})
			case "negative baseline":
				if err := db.Model(&model.NodeClientTraffic{}).Where("node_id = ? AND email = ?", 1, "alice").Update("up", -1).Error; err != nil {
					t.Fatal(err)
				}
			case "negative local":
				if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", "alice").Update("up", -1).Error; err != nil {
					t.Fatal(err)
				}
			}
			if _, err := svc.setRemoteTrafficLocked(1, snap, false); err == nil {
				t.Fatal("invalid node snapshot accepted")
			}
			var baseline model.NodeClientTraffic
			if err := db.Where("node_id = ? AND email = ?", 1, "alice").First(&baseline).Error; err != nil {
				t.Fatal(err)
			}
			expected := int64(100)
			if kind == "negative baseline" {
				expected = -1
			}
			if baseline.Up != expected || baseline.Down != 90 {
				t.Fatalf("baseline changed: %+v", baseline)
			}
			row := readTraffic(t, db, "alice")
			if kind == "negative local" {
				expected = -1
			} else {
				expected = 100
			}
			if row.Up != expected || row.Down != 90 {
				t.Fatalf("traffic changed: %+v", row)
			}
		})
	}
}

func TestNodeTrafficDeltaSaturatesInsteadOfOverflowing(t *testing.T) {
	db := initTrafficTestDB(t)
	createNodeInbound(t, db, 1, "node-overflow", 41202)
	svc := &InboundService{}
	syncNode(t, svc, 1, "node-overflow", xray.ClientTraffic{Email: "alice", Up: 100, Down: 90, Enable: true})
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", "alice").Update("up", int64(math.MaxInt64-5)).Error; err != nil {
		t.Fatal(err)
	}
	syncNode(t, svc, 1, "node-overflow", xray.ClientTraffic{Email: "alice", Up: 120, Down: 100, Enable: true})
	row := readTraffic(t, db, "alice")
	if row.Up != math.MaxInt64 || row.Down != 100 {
		t.Fatalf("counter overflowed: %+v", row)
	}
}

func TestNodeTrafficTotalComparisonHandlesOverflow(t *testing.T) {
	if !nodeTrafficTotalLE(math.MaxInt64-1, 20, math.MaxInt64, 20) {
		t.Fatal("ordered high totals compared incorrectly")
	}
	if nodeTrafficTotalLE(math.MaxInt64, 20, math.MaxInt64-1, 20) {
		t.Fatal("higher total compared as lower")
	}
}
