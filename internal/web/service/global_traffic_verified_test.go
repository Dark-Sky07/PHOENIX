package service

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestVerifiedGlobalTrafficAtomicReplayAndIdentity(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Create(&model.TrafficSnapshotReceiver{ID: "trusted-peer", Sender: "master", Generation: "epoch"}).Error; err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x52}, 32)
	makeEnvelope := func(seq uint64, sender, generation string, up int64) trafficprotocol.Envelope {
		t.Helper()
		payload, err := json.Marshal([]*xray.ClientTraffic{{Email: client.Email, Up: up, Down: 10}})
		if err != nil {
			t.Fatal(err)
		}
		e, err := trafficprotocol.Sign(trafficprotocol.Envelope{Version: trafficprotocol.Version, Sender: sender, Generation: generation, Sequence: seq, Snapshot: payload}, key)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	svc := &InboundService{}
	e := makeEnvelope(1, "master", "epoch", 100)
	if err := svc.AcceptVerifiedGlobalTraffic("trusted-peer", bytes.Repeat([]byte{0x53}, 32), e); err == nil {
		t.Fatal("wrong key accepted")
	}
	if err := svc.AcceptVerifiedGlobalTraffic("trusted-peer", key, makeEnvelope(1, "attacker", "epoch", 100)); err == nil {
		t.Fatal("unregistered sender accepted")
	}
	if err := svc.AcceptVerifiedGlobalTraffic("trusted-peer", key, makeEnvelope(1, "master", "other", 100)); err == nil {
		t.Fatal("unregistered generation accepted")
	}
	if err := svc.AcceptVerifiedGlobalTraffic("trusted-peer", key, e); err != nil {
		t.Fatal(err)
	}
	if err := svc.AcceptVerifiedGlobalTraffic("trusted-peer", key, e); err == nil {
		t.Fatal("replay accepted")
	}
	var row model.ClientGlobalTraffic
	if err := db.Where("master_guid = ? AND email = ?", "master", client.Email).First(&row).Error; err != nil || row.Up != 100 {
		t.Fatalf("stored global traffic: %+v %v", row, err)
	}
	// Failure in the accounting write must also roll back the sequence CAS.
	if err := db.Exec("CREATE TRIGGER reject_verified_global BEFORE UPDATE ON client_global_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	next := makeEnvelope(2, "master", "epoch", 200)
	if err := svc.AcceptVerifiedGlobalTraffic("trusted-peer", key, next); err == nil {
		t.Fatal("expected accounting failure")
	}
	var receiver model.TrafficSnapshotReceiver
	if err := db.First(&receiver, "id = ?", "trusted-peer").Error; err != nil || receiver.LastSequence != 1 {
		t.Fatalf("sequence did not roll back: %+v %v", receiver, err)
	}
	if err := db.Exec("DROP TRIGGER reject_verified_global").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AcceptVerifiedGlobalTraffic("trusted-peer", key, next); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&receiver, "id = ?", "trusted-peer").Error; err != nil || receiver.LastSequence != 2 {
		t.Fatalf("sequence not updated: %+v %v", receiver, err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 200 {
		t.Fatalf("snapshot not applied: %+v %v", row, err)
	}
}
