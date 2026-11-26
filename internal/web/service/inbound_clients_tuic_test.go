package service

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
)

func TestBuildTargetClientFromSourceTuic(t *testing.T) {
	s := &InboundService{}
	source := model.Client{
		Email:    "test@example.com",
		ID:       "old-uuid",
		Password: "old-password",
	}
	targetInbound := &model.Inbound{
		Protocol: model.TUIC,
	}

	target, err := s.buildTargetClientFromSource(source, targetInbound, "test@example.com", "")
	if err != nil {
		t.Fatalf("buildTargetClientFromSource failed: %v", err)
	}

	if target.ID == "" || target.ID == "old-uuid" {
		t.Fatalf("expected new UUID for TUIC client, got %q", target.ID)
	}
	if target.Password == "" || target.Password == "old-password" {
		t.Fatalf("expected new password for TUIC client, got %q", target.Password)
	}
}

func TestAddInboundTuicClientValidation(t *testing.T) {
	setupConflictDB(t)
	s := &InboundService{}
	ib := &model.Inbound{
		Tag:      "tuic-test-1",
		Protocol: model.TUIC,
		Settings: `{"clients":[{"id":"uuid-1","password":""}]}`,
	}
	_, _, err := s.AddInbound(ib)
	if err == nil || strings.TrimSpace(err.Error()) != "tuic client requires a password" {
		t.Fatalf("expected 'tuic client requires a password' error, got %v", err)
	}

	ibNoID := &model.Inbound{
		Tag:      "tuic-test-2",
		Protocol: model.TUIC,
		Settings: `{"clients":[{"id":"","password":"pass"}]}`,
	}
	_, _, errNoID := s.AddInbound(ibNoID)
	if errNoID == nil || strings.TrimSpace(errNoID.Error()) != "empty client ID" {
		t.Fatalf("expected 'empty client ID' error, got %v", errNoID)
	}

	ibNoEmail := &model.Inbound{
		Tag:      "tuic-test-3",
		Protocol: model.TUIC,
		Settings: `{"clients":[{"id":"uuid-3","password":"pass","email":""}]}`,
	}
	_, _, errNoEmail := s.AddInbound(ibNoEmail)
	if errNoEmail == nil || strings.TrimSpace(errNoEmail.Error()) != "empty client email" {
		t.Fatalf("expected 'empty client email' error, got %v", errNoEmail)
	}
}

func TestTuicClientMutationsDoNotRequireRestart(t *testing.T) {
	setupConflictDB(t)
	mgr := runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return 0 }, SetNeedRestart: func() {}})
	runtime.SetManager(mgr)
	t.Cleanup(func() { runtime.SetManager(nil) })

	inboundSvc := &InboundService{}
	clientSvc := &ClientService{}

	cert, key := testTUICCertificate(t)
	settings, err := json.Marshal(map[string]any{
		"certificate": cert, "private_key": key,
		"clients": []model.Client{{ID: "a0000000-0000-0000-0000-000000000001", Password: "p1", Email: "user1@tuic.com", Enable: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	ib := &model.Inbound{
		Tag: "tuic-restart-test", Protocol: model.TUIC, Listen: "127.0.0.1",
		Port: port, Enable: true, Settings: string(settings),
	}
	created, _, err := inboundSvc.AddInbound(ib)
	if err != nil {
		t.Fatalf("AddInbound failed: %v", err)
	}
	t.Cleanup(func() { tuic.GetManager().Remove(created.Id) })

	addPayload := &model.Inbound{
		Id:       created.Id,
		Settings: `{"clients":[{"id":"a0000000-0000-0000-0000-000000000002","password":"p2","email":"user2@tuic.com","enable":true}]}`,
	}
	needRestart, err := clientSvc.AddInboundClient(inboundSvc, addPayload)
	if err != nil {
		t.Fatalf("AddInboundClient failed: %v", err)
	}
	if needRestart {
		t.Fatal("expected needRestart = false when adding TUIC client")
	}

	updatePayload := &model.Inbound{
		Id:       created.Id,
		Settings: `{"clients":[{"id":"a0000000-0000-0000-0000-000000000002","password":"new-p2","email":"user2@tuic.com","enable":true}]}`,
	}
	needRestart, err = clientSvc.UpdateInboundClient(inboundSvc, updatePayload, "user2@tuic.com")
	if err != nil {
		t.Fatalf("UpdateInboundClient failed: %v", err)
	}
	if needRestart {
		t.Fatal("expected needRestart = false when updating TUIC client")
	}

	needRestart, err = clientSvc.DelInboundClientByEmail(inboundSvc, created.Id, "user2@tuic.com", false)
	if err != nil {
		t.Fatalf("DelInboundClientByEmail failed: %v", err)
	}
	if needRestart {
		t.Fatal("expected needRestart = false when deleting TUIC client")
	}
}
