package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestVerifiedTrafficPeerRegistrationIsExplicitAndNeverResets(t *testing.T) {
	db := initTrafficTestDB(t)
	token := model.ApiToken{Name: "peer-token", Token: "hash", Enabled: true, Scope: "full"}
	if err := db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	if err := RegisterVerifiedTrafficReceiver(token.Id, "master", "generation", false); err == nil {
		t.Fatal("missing reconciliation accepted")
	}
	if err := RegisterVerifiedTrafficReceiver(token.Id, "master", "generation", true); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.TrafficSnapshotReceiver{}).Where("sender = ?", "master").Update("last_sequence", 17).Error; err != nil {
		t.Fatal(err)
	}
	if err := RegisterVerifiedTrafficReceiver(token.Id, "master", "new-generation", true); err == nil {
		t.Fatal("registration reset an existing stream")
	}
	other := model.ApiToken{Name: "other-token", Token: "other-hash", Enabled: true, Scope: "full"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	if err := RegisterVerifiedTrafficReceiver(other.Id, "master", "other", true); err == nil {
		t.Fatal("sender acquired two sequence streams")
	}
	var receiver model.TrafficSnapshotReceiver
	if err := db.Where("sender = ?", "master").First(&receiver).Error; err != nil || receiver.Generation != "generation" || receiver.LastSequence != 17 {
		t.Fatalf("receiver changed: %+v %v", receiver, err)
	}
	node := model.Node{Name: "peer", Address: "example.com", Port: 443, Scheme: "https", TlsVerifyMode: "verify", ApiToken: "0123456789012345678901234567890123456789", Enable: true}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterVerifiedTrafficSender(node.Id, false); err == nil {
		t.Fatal("missing confirmation accepted")
	}
	state, err := RegisterVerifiedTrafficSender(node.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	if state.Sender == "" || state.Generation == "" {
		t.Fatalf("invalid state: %+v", state)
	}
	if err := db.First(&node, node.Id).Error; err != nil || !node.VerifiedGlobalPush {
		t.Fatalf("verified mode not enabled: %+v %v", node, err)
	}
	if err := db.Model(&model.TrafficSnapshotSender{}).Where("id = ?", state.ID).Update("last_sequence", 8).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterVerifiedTrafficSender(node.Id, true); err == nil {
		t.Fatal("existing sender reset")
	}
	var saved model.TrafficSnapshotSender
	if err := db.First(&saved, "id = ?", state.ID).Error; err != nil || saved.LastSequence != 8 || saved.Generation != state.Generation {
		t.Fatalf("sender changed: %+v %v", saved, err)
	}
}

func TestVerifiedTrafficRegistrationRejectsInvalidCredentials(t *testing.T) {
	db := initTrafficTestDB(t)
	token := model.ApiToken{Name: "readonly", Token: "hash", Enabled: true, Scope: "read"}
	if err := db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	if err := RegisterVerifiedTrafficReceiver(token.Id, "master", "generation", true); err == nil {
		t.Fatal("read-only token accepted")
	}
	if err := RegisterVerifiedTrafficReceiver(token.Id, " master", "generation", true); err == nil {
		t.Fatal("noncanonical sender accepted")
	}
	node := model.Node{Name: "insecure", Address: "example.com", Port: 80, Scheme: "http", ApiToken: "0123456789012345678901234567890123456789", Enable: true}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterVerifiedTrafficSender(node.Id, true); err == nil {
		t.Fatal("insecure node accepted")
	}
	var count int64
	if err := db.Model(&model.TrafficSnapshotSender{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unexpected sender state: %d %v", count, err)
	}
}
