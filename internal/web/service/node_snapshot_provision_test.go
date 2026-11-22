package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
)

func TestNodeSnapshotProvisioningDisablesLegacyWithoutReset(t *testing.T) {
	db := initTrafficTestDB(t)
	token := model.ApiToken{Name: "snapshot-token", Token: "hash", Scope: "full", Enabled: true}
	if err := db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterNodeSnapshotProducer(token.Id, false); err == nil {
		t.Fatal("unconfirmed producer accepted")
	}
	producer, err := RegisterNodeSnapshotProducer(token.Id, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RegisterNodeSnapshotProducer(token.Id, true); err == nil {
		t.Fatal("producer reset accepted")
	}
	node := model.Node{Name: "snapshot-node", Address: "example.com", Port: 443, Scheme: "https", ApiToken: "0123456789012345678901234567890123456789"}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if err := RegisterNodeSnapshotReceiver(node.Id, producer.Sender, producer.Generation, false); err == nil {
		t.Fatal("unconfirmed receiver accepted")
	}
	if err := RegisterNodeSnapshotReceiver(node.Id, producer.Sender, producer.Generation, true); err != nil {
		t.Fatal(err)
	}
	if err := RegisterNodeSnapshotReceiver(node.Id, producer.Sender, "replacement", true); err == nil {
		t.Fatal("receiver reset accepted")
	}
	required, err := NodeInboundVerificationRequired(node.Id)
	if err != nil || !required {
		t.Fatalf("receiver mode: %v %v", required, err)
	}
	if _, err := (&InboundService{}).setRemoteTrafficLocked(node.Id, &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{}}, false); err == nil {
		t.Fatal("legacy merge accepted after registration")
	}
}
