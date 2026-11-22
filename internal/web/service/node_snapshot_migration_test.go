package service

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// Separate SQLite databases model a node and a master without pretending this
// single-process test exercises HTTPS, cross-host locks, or unpolled traffic.
func TestNodeSnapshotMigrationPreservesBaselineAndRejectsDowngrade(t *testing.T) {
	nodeDir, masterDir := t.TempDir(), t.TempDir()
	t.Setenv("XUI_DB_TYPE", "sqlite")
	t.Setenv("XUI_DB_FOLDER", nodeDir)
	if err := database.InitDB(filepath.Join(nodeDir, "node.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	var user model.User
	if err := db.Order("id asc").First(&user).Error; err != nil {
		t.Fatal(err)
	}
	tokenID := model.ApiToken{Name: "master-peer", Token: "hash", Scope: "full", Enabled: true}
	if err := db.Create(&tokenID).Error; err != nil {
		t.Fatal(err)
	}
	producer, err := RegisterNodeSnapshotProducer(tokenID.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	ib := model.Inbound{UserId: user.Id, Tag: "migration-inbound", Port: 47441, Protocol: model.VLESS, Settings: `{"clients":[{"email":"migration@example.test","enable":true}]}`}
	if err := db.Create(&ib).Error; err != nil {
		t.Fatal(err)
	}
	stats := xray.ClientTraffic{InboundId: ib.Id, Email: "migration@example.test", Up: 100, Down: 10, Enable: true}
	if err := db.Create(&stats).Error; err != nil {
		t.Fatal(err)
	}
	token := "0123456789012345678901234567890123456789"
	svc := &InboundService{}
	first, err := svc.PrepareVerifiedInboundSnapshot(tokenID.Id, user.Id, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&stats).Updates(map[string]any{"up": 150, "down": 20}).Error; err != nil {
		t.Fatal(err)
	}
	second, err := svc.PrepareVerifiedInboundSnapshot(tokenID.Id, user.Id, token)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CloseDB(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XUI_DB_FOLDER", masterDir)
	if err := database.InitDB(filepath.Join(masterDir, "master.db")); err != nil {
		t.Fatal(err)
	}
	db = database.GetDB()
	node := model.Node{Name: "migration-node", Address: "example.com", Port: 443, Scheme: "https", ApiToken: token}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	createNodeInbound(t, db, node.Id, "migration-inbound", 47441)
	var legacy runtime.TrafficSnapshot
	if err := json.Unmarshal(first.Snapshot, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.setRemoteTrafficLocked(node.Id, &legacy, false); err != nil {
		t.Fatal(err)
	}
	if err := RegisterNodeSnapshotReceiver(node.Id, producer.Sender, producer.Generation, true); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.SetVerifiedRemoteTraffic(node.Id, *second)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	traffic := readTraffic(t, db, stats.Email)
	if traffic.Up != 150 || traffic.Down != 20 {
		t.Fatalf("migration lost or duplicated usage: %+v", traffic)
	}
	if _, err := svc.setRemoteTrafficLocked(node.Id, &legacy, false); err == nil {
		t.Fatal("legacy downgrade accepted")
	}
	if _, err := svc.acceptVerifiedNodeTrafficLocked(node.Id, *first); err == nil {
		t.Fatal("older snapshot accepted")
	}
	if _, err := svc.acceptVerifiedNodeTrafficLocked(node.Id, *second); err == nil {
		t.Fatal("snapshot replay accepted")
	}
}
