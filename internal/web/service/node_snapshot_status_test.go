package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestNodeSnapshotSafetyStatusIncludesAllNodesAndFailsClosed(t *testing.T) {
	db := initTrafficTestDB(t)
	if status := readNodeSnapshotSafetyStatus(db); !status.Available || status.NodeCount != 0 {
		t.Fatalf("empty: %+v", status)
	}
	for _, node := range []model.Node{{Name: "legacy", Address: "legacy.example", Port: 443, Enable: true}, {Name: "verified-outbound-only", Address: "new.example", Port: 443, Enable: false, VerifiedGlobalPush: true}} {
		if err := db.Create(&node).Error; err != nil {
			t.Fatal(err)
		}
	}
	if status := readNodeSnapshotSafetyStatus(db); !status.Available || status.NodeCount != 2 {
		t.Fatalf("node safety: %+v", status)
	}
	if status := readNodeSnapshotSafetyStatus(nil); status.Available {
		t.Fatalf("nil database reported clear: %+v", status)
	}
	if err := db.Migrator().DropTable(&model.Node{}); err != nil {
		t.Fatal(err)
	}
	if status := readNodeSnapshotSafetyStatus(db); status.Available {
		t.Fatalf("database error reported clear: %+v", status)
	}
}
