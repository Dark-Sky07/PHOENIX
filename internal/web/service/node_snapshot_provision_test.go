package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
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
	key, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(node.ApiToken)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := trafficprotocol.Sign(trafficprotocol.Envelope{Version: trafficprotocol.Version, Sender: producer.Sender, Generation: producer.Generation, Sequence: 1, Snapshot: []byte(`{"inbounds":[]}`)}, key)
	if err != nil {
		t.Fatal(err)
	}
	probe := func(context.Context, *model.Node) (*trafficprotocol.Envelope, error) { return &proof, nil }
	failed := func(context.Context, *model.Node) (*trafficprotocol.Envelope, error) {
		return nil, errors.New("not ready")
	}
	if err := registerNodeSnapshotReceiver(context.Background(), node.Id, producer.Sender, producer.Generation, true, failed); err == nil {
		t.Fatal("unready producer accepted")
	}
	required, err := NodeInboundVerificationRequired(node.Id)
	if err != nil || required {
		t.Fatalf("failed probe switched mode: %v %v", required, err)
	}
	if err := registerNodeSnapshotReceiver(context.Background(), node.Id, producer.Sender, producer.Generation, false, probe); err == nil {
		t.Fatal("unconfirmed receiver accepted")
	}
	badProof := proof
	badProof.MAC = append([]byte(nil), proof.MAC...)
	badProof.MAC[0] ^= 1
	badProbe := func(context.Context, *model.Node) (*trafficprotocol.Envelope, error) { return &badProof, nil }
	if err := registerNodeSnapshotReceiver(context.Background(), node.Id, producer.Sender, producer.Generation, true, badProbe); err == nil {
		t.Fatal("invalid authentication accepted")
	}
	if err := registerNodeSnapshotReceiver(context.Background(), node.Id, producer.Sender, "wrong-generation", true, probe); err == nil {
		t.Fatal("incorrect stream generation accepted")
	}
	if err := registerNodeSnapshotReceiver(context.Background(), node.Id, producer.Sender, producer.Generation, true, probe); err != nil {
		t.Fatal(err)
	}
	var receiver model.TrafficSnapshotReceiver
	if err := db.First(&receiver, "id = ?", fmt.Sprintf("node-inbound:%d", node.Id)).Error; err != nil || receiver.LastSequence != int64(proof.Sequence) {
		t.Fatalf("probe sequence not fenced: %+v %v", receiver, err)
	}
	if err := registerNodeSnapshotReceiver(context.Background(), node.Id, producer.Sender, "replacement", true, probe); err == nil {
		t.Fatal("receiver reset accepted")
	}
	required, err = NodeInboundVerificationRequired(node.Id)
	if err != nil || !required {
		t.Fatalf("receiver mode: %v %v", required, err)
	}
	if _, err := (&InboundService{}).setRemoteTrafficLocked(node.Id, &runtime.TrafficSnapshot{Inbounds: []*model.Inbound{}}, false); err == nil {
		t.Fatal("legacy merge accepted after registration")
	}
}

func TestNodeSnapshotReceiverRejectsChangedNodeConfiguration(t *testing.T) {
	db := initTrafficTestDB(t)
	node := model.Node{Name: "node-race", Address: "example.com", Port: 443, Scheme: "https", ApiToken: "0123456789012345678901234567890123456789"}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	key, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(node.ApiToken)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := trafficprotocol.Sign(trafficprotocol.Envelope{Version: trafficprotocol.Version, Sender: "node", Generation: "g", Sequence: 1, Snapshot: []byte("{}")}, key)
	if err != nil {
		t.Fatal(err)
	}
	probe := func(context.Context, *model.Node) (*trafficprotocol.Envelope, error) {
		if err := db.Model(&model.Node{}).Where("id = ?", node.Id).Update("address", "changed.example.com").Error; err != nil {
			return nil, err
		}
		return &proof, nil
	}
	if err := registerNodeSnapshotReceiver(context.Background(), node.Id, "node", "g", true, probe); err == nil {
		t.Fatal("changed configuration accepted")
	}
	if required, err := NodeInboundVerificationRequired(node.Id); err != nil || required {
		t.Fatalf("mode changed after race: %v %v", required, err)
	}
}

func TestNodeSnapshotReceiverRejectsDirtyConfiguration(t *testing.T) {
	db := initTrafficTestDB(t)
	node := model.Node{Name: "node-dirty", Address: "example.com", Port: 443, Scheme: "https", ApiToken: "0123456789012345678901234567890123456789", ConfigDirty: true}
	if err := db.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	called := false
	probe := func(context.Context, *model.Node) (*trafficprotocol.Envelope, error) { called = true; return nil, nil }
	if err := registerNodeSnapshotReceiver(context.Background(), node.Id, "node", "g", true, probe); err == nil || called {
		t.Fatal("dirty node was probed or accepted")
	}
	if required, err := NodeInboundVerificationRequired(node.Id); err != nil || required {
		t.Fatalf("mode changed: %v %v", required, err)
	}
}
