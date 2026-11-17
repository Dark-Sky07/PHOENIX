package service

import (
	"math"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestPPPAbsoluteCountersAreTransactionalAndIdempotent(t *testing.T) {
	setupBulkDB(t)
	inbound := mkInbound(t, 32123, model.PPTP, `{"clients":[]}`)
	client := model.Client{
		Email:   "ppp-user@example.test",
		ID:      "d271c1dc-664c-4f48-8f86-8f973c68e70c",
		SubID:   "ppp-user-sub",
		TotalGB: 300,
		Enable:  true,
	}
	inbound.Settings = clientsSettings(t, []model.Client{client})
	if err := database.GetDB().Save(inbound).Error; err != nil {
		t.Fatalf("save PPTP settings: %v", err)
	}
	if err := (&ClientService{}).SyncInbound(nil, inbound.Id, []model.Client{client}); err != nil {
		t.Fatalf("seed client linkage: %v", err)
	}
	if err := database.GetDB().Create(&xray.ClientTraffic{InboundId: inbound.Id, Email: client.Email, Total: client.TotalGB, Enable: true}).Error; err != nil {
		t.Fatalf("seed client traffic row: %v", err)
	}

	service := &InboundService{}
	apply := func(rx, tx int64) {
		t.Helper()
		_, _, err := service.AddTraffic(nil, []*xray.ClientTraffic{{
			Email:        client.Email,
			Enable:       true,
			PPPSessionID: "pptp-session-uuid",
			PPPInboundID: inbound.Id,
			PPPRxBytes:   rx,
			PPPTxBytes:   tx,
		}})
		if err != nil {
			t.Fatalf("apply PPP counters rx=%d tx=%d: %v", rx, tx, err)
		}
	}
	assertUsage := func(wantUp, wantDown int64) {
		t.Helper()
		var row xray.ClientTraffic
		if err := database.GetDB().Where("email = ?", client.Email).Take(&row).Error; err != nil {
			t.Fatalf("read client usage: %v", err)
		}
		if row.Up != wantUp || row.Down != wantDown {
			t.Fatalf("client usage up/down = %d/%d, want %d/%d", row.Up, row.Down, wantUp, wantDown)
		}
	}

	// Force an error after the absolute checkpoint is staged in the transaction;
	// neither the checkpoint nor any usage may survive that rollback.
	if _, _, err := service.AddTraffic(nil, []*xray.ClientTraffic{
		{Email: client.Email, Enable: true, PPPSessionID: "pptp-session-uuid", PPPInboundID: inbound.Id, PPPRxBytes: 100, PPPTxBytes: 200},
		{Email: "invalid@example.test", Up: -1},
	}); err == nil {
		t.Fatal("invalid delta should abort the whole traffic transaction")
	}
	assertUsage(0, 0)
	if err := database.GetDB().Where("session_id = ?", "pptp-session-uuid").Take(&model.PPPClientTrafficCounter{}).Error; err == nil {
		t.Fatal("checkpoint update survived a rolled-back usage transaction")
	}
	apply(100, 200)
	assertUsage(100, 200)
	var depleted xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", client.Email).Take(&depleted).Error; err != nil {
		t.Fatal(err)
	}
	if depleted.Enable {
		t.Fatal("client at its exact traffic quota was not disabled")
	}
	apply(100, 200) // replay after a lost acknowledgement must be a no-op
	assertUsage(100, 200)
	apply(145, 260)
	assertUsage(145, 260)
	apply(10, 5) // a kernel counter reset must not subtract already-accounted usage
	assertUsage(155, 265)

	var checkpoint model.PPPClientTrafficCounter
	if err := database.GetDB().Where("session_id = ?", "pptp-session-uuid").Take(&checkpoint).Error; err != nil {
		t.Fatalf("read PPP checkpoint: %v", err)
	}
	if checkpoint.RxBytes != 10 || checkpoint.TxBytes != 5 || checkpoint.InboundID != inbound.Id || checkpoint.Email != client.Email {
		t.Fatalf("unexpected persisted checkpoint: %+v", checkpoint)
	}
}

func TestTrafficAccountingSaturatesAndRejectsNegativeDeltas(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	inbound := mkInbound(t, 32124, model.PPTP, `{"clients":[]}`)
	client := model.Client{Email: "saturated@example.test", ID: "11f1ea5b-0313-4987-a179-c66cb7efde6f", Enable: true}
	inbound.Settings = clientsSettings(t, []model.Client{client})
	if err := db.Save(inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&ClientService{}).SyncInbound(nil, inbound.Id, []model.Client{client}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{InboundId: inbound.Id, Email: client.Email, Up: math.MaxInt64 - 10, Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	service := &InboundService{}
	_, _, err := service.AddTraffic(nil, []*xray.ClientTraffic{{
		Email: client.Email, Enable: true, PPPSessionID: "saturated-session",
		PPPInboundID: inbound.Id, PPPRxBytes: 100, PPPTxBytes: 200,
	}})
	if err != nil {
		t.Fatal(err)
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != math.MaxInt64 || row.Down != 200 {
		t.Fatalf("traffic counters wrapped instead of saturating: up=%d down=%d", row.Up, row.Down)
	}
	if _, _, err := service.AddTraffic(nil, []*xray.ClientTraffic{{Email: client.Email, Up: -1}}); err == nil {
		t.Fatal("negative client traffic delta must be rejected")
	}
	if err := db.Where("email = ?", client.Email).Take(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != math.MaxInt64 || row.Down != 200 {
		t.Fatalf("rejected delta changed persisted usage: up=%d down=%d", row.Up, row.Down)
	}
}

func TestPPPCheckpointCannotBeReassignedToDifferentIdentity(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	inbound := mkInbound(t, 32125, model.PPTP, `{"clients":[]}`)
	first := model.Client{Email: "first@example.test", ID: "11f1ea5b-0313-4987-a179-c66cb7efde60", Enable: true}
	second := model.Client{Email: "second@example.test", ID: "11f1ea5b-0313-4987-a179-c66cb7efde61", Enable: true}
	inbound.Settings = clientsSettings(t, []model.Client{first, second})
	if err := db.Save(inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&ClientService{}).SyncInbound(nil, inbound.Id, []model.Client{first, second}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create([]xray.ClientTraffic{
		{InboundId: inbound.Id, Email: first.Email, Enable: true},
		{InboundId: inbound.Id, Email: second.Email, Enable: true},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.PPPClientTrafficCounter{SessionID: "reused-session", Email: first.Email, InboundID: inbound.Id, RxBytes: 100, TxBytes: 200}).Error; err != nil {
		t.Fatal(err)
	}
	_, _, err := (&InboundService{}).AddTraffic(nil, []*xray.ClientTraffic{{
		Email: second.Email, Enable: true, PPPSessionID: "reused-session",
		PPPInboundID: inbound.Id, PPPRxBytes: 150, PPPTxBytes: 250,
	}})
	if err == nil {
		t.Fatal("PPP session checkpoint was silently reassigned to another account")
	}
	var checkpoint model.PPPClientTrafficCounter
	if err := db.Where("session_id = ?", "reused-session").Take(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	if checkpoint.Email != first.Email || checkpoint.RxBytes != 100 || checkpoint.TxBytes != 200 {
		t.Fatalf("failed reassignment changed original checkpoint: %+v", checkpoint)
	}
}
