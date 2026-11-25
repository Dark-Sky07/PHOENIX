package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"

	"github.com/google/uuid"
)

func TestOpenVPSessionCountersAreIdempotent(t *testing.T) {
	setupBulkDB(t)
	inbound := mkInbound(t, 32444, model.OpenVPN, `{"clients":[]}`)
	client := model.Client{
		Email:   "openvpn-session@example.test",
		ID:      "0e1f2a3b-4c5d-4e6f-8a9b-0c1d2e3f4a5b",
		SubID:   "openvpn-session-sub",
		TotalGB: 300,
		Enable:  true,
	}
	inbound.Settings = clientsSettings(t, []model.Client{client})
	if err := database.GetDB().Save(inbound).Error; err != nil {
		t.Fatalf("save OpenVPN settings: %v", err)
	}
	if err := (&ClientService{}).SyncInbound(nil, inbound.Id, []model.Client{client}); err != nil {
		t.Fatalf("seed client linkage: %v", err)
	}
	if err := database.GetDB().Create(&xray.ClientTraffic{InboundId: inbound.Id, Email: client.Email, Total: client.TotalGB, Enable: true}).Error; err != nil {
		t.Fatalf("seed client traffic row: %v", err)
	}

	marker := uuid.NewString()
	if err := BeginXrayPoll(marker); err != nil {
		t.Fatal(err)
	}
	sample := func(rx, tx int64) []*xray.ClientTraffic {
		return []*xray.ClientTraffic{{
			Email: client.Email, Enable: true,
			OpenVPNSessionID: "openvpn:32444:generation:session", OpenVPNInboundID: inbound.Id,
			OpenVPNRxBytes: rx, OpenVPNTxBytes: tx,
		}}
	}
	service := &InboundService{}
	if _, _, err := service.AddOpenVPNSessionTrafficUnderAllocationLock(marker, sample(100, 200)); err != nil {
		t.Fatalf("commit final session: %v", err)
	}
	assertUsage := func(wantUp, wantDown int64) {
		t.Helper()
		var row xray.ClientTraffic
		if err := database.GetDB().Where("email = ?", client.Email).Take(&row).Error; err != nil {
			t.Fatal(err)
		}
		if row.Up != wantUp || row.Down != wantDown {
			t.Fatalf("usage = %d/%d, want %d/%d", row.Up, row.Down, wantUp, wantDown)
		}
	}
	assertUsage(100, 200)

	// A replay after an uncertain commit, restart or duplicated hook record
	// must not charge the same final counters again.
	if _, _, err := service.AddOpenVPNSessionTrafficUnderAllocationLock(marker, sample(100, 200)); err != nil {
		t.Fatalf("replay final session: %v", err)
	}
	assertUsage(100, 200)

	if _, _, err := service.AddOpenVPNSessionTrafficUnderAllocationLock(marker, sample(90, 200)); err == nil {
		t.Fatal("decreased session counter accepted")
	}
	assertUsage(100, 200)
	if err := FinishXrayPoll(marker); err != nil {
		t.Fatal(err)
	}
}

func TestOpenVPSessionTrafficRequiresMarker(t *testing.T) {
	service := &InboundService{}
	if _, _, err := service.AddOpenVPNSessionTrafficUnderAllocationLock("", []*xray.ClientTraffic{{
		Email: "a@example.test", OpenVPNSessionID: "session", OpenVPNInboundID: 1,
	}}); err == nil {
		t.Fatal("missing marker accepted")
	}
}
