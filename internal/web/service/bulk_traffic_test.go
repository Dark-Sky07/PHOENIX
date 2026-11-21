package service

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func mkTraffic(t *testing.T, inboundId int, email string, up, down, total, expiry int64, enable bool) {
	t.Helper()
	row := xray.ClientTraffic{
		InboundId:  inboundId,
		Email:      email,
		Up:         up,
		Down:       down,
		Total:      total,
		ExpiryTime: expiry,
		Enable:     enable,
	}
	if err := database.GetDB().Create(&row).Error; err != nil {
		t.Fatalf("create traffic %s: %v", email, err)
	}
}

func trafficOf(t *testing.T, email string) xray.ClientTraffic {
	t.Helper()
	var row xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", email).First(&row).Error; err != nil {
		t.Fatalf("load traffic %s: %v", email, err)
	}
	return row
}

func TestBulkResetTrafficZeroesUsageAndReenables(t *testing.T) {
	setupBulkDB(t)
	svc := &ClientService{}
	inboundSvc := &InboundService{}

	source := []model.Client{
		{Email: "alice@x", ID: "11111111-1111-1111-1111-111111111111", SubID: "sa", Enable: true},
		{Email: "bob@x", ID: "22222222-2222-2222-2222-222222222222", SubID: "sb", Enable: true},
		{Email: "carol@x", ID: "33333333-3333-3333-3333-333333333333", SubID: "sc", Enable: true},
	}
	ib := mkInbound(t, 21001, model.VLESS, clientsSettings(t, source))
	if err := svc.SyncInbound(nil, ib.Id, source); err != nil {
		t.Fatalf("seed linkage: %v", err)
	}
	mkTraffic(t, ib.Id, "alice@x", 10, 20, 0, 0, false)
	mkTraffic(t, ib.Id, "bob@x", 5, 5, 0, 0, true)
	mkTraffic(t, ib.Id, "carol@x", 7, 0, 0, 0, true)

	affected, err := svc.BulkResetTraffic(inboundSvc, []string{"alice@x", "bob@x"})
	if err != nil {
		t.Fatalf("BulkResetTraffic: %v", err)
	}
	if affected != 2 {
		t.Fatalf("expected 2 affected, got %d", affected)
	}

	for _, e := range []string{"alice@x", "bob@x"} {
		tr := trafficOf(t, e)
		if tr.Up != 0 || tr.Down != 0 {
			t.Fatalf("%s: expected up/down 0, got up=%d down=%d", e, tr.Up, tr.Down)
		}
		if !tr.Enable {
			t.Fatalf("%s: expected re-enabled", e)
		}
	}

	carol := trafficOf(t, "carol@x")
	if carol.Up != 7 {
		t.Fatalf("carol not in list should be untouched, got up=%d", carol.Up)
	}
}

func TestDelDepletedRemovesOnlyDepleted(t *testing.T) {
	setupBulkDB(t)
	svc := &ClientService{}
	inboundSvc := &InboundService{}

	source := []model.Client{
		{Email: "alice@x", ID: "11111111-1111-1111-1111-111111111111", SubID: "sa", Enable: true},
		{Email: "bob@x", ID: "22222222-2222-2222-2222-222222222222", SubID: "sb", Enable: true},
		{Email: "carol@x", ID: "33333333-3333-3333-3333-333333333333", SubID: "sc", Enable: true},
	}
	ib := mkInbound(t, 21002, model.VLESS, clientsSettings(t, source))
	if err := svc.SyncInbound(nil, ib.Id, source); err != nil {
		t.Fatalf("seed linkage: %v", err)
	}
	past := time.Now().Add(-time.Hour).UnixMilli()
	mkTraffic(t, ib.Id, "alice@x", 60, 60, 100, 0, true)
	mkTraffic(t, ib.Id, "bob@x", 10, 10, 100, 0, true)
	mkTraffic(t, ib.Id, "carol@x", 0, 0, 0, past, true)

	deleted, _, err := svc.DelDepleted(inboundSvc)
	if err != nil {
		t.Fatalf("DelDepleted: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 deleted (alice traffic-depleted, carol expired), got %d", deleted)
	}

	if _, err := svc.GetRecordByEmail(nil, "bob@x"); err != nil {
		t.Fatalf("bob should survive: %v", err)
	}
	for _, e := range []string{"alice@x", "carol@x"} {
		if _, err := svc.GetRecordByEmail(nil, e); err == nil {
			t.Fatalf("%s should be deleted", e)
		}
	}

	reloaded, _ := inboundSvc.GetInbound(ib.Id)
	jsonClients, _ := inboundSvc.GetClients(reloaded)
	if len(jsonClients) != 1 || jsonClients[0].Email != "bob@x" {
		t.Fatalf("settings JSON should contain only bob, got %d clients", len(jsonClients))
	}
}

func TestDelDepletedClientsKeepsInboundAndCommitsBeforeRuntime(t *testing.T) {
	setupBulkDB(t)
	inboundSvc := &InboundService{}
	clientSvc := &ClientService{}

	client := model.Client{
		Email:  "expired@x",
		ID:     "44444444-4444-4444-4444-444444444444",
		SubID:  "expired-sub",
		Enable: true,
	}
	ib := mkInbound(t, 21004, model.VLESS, clientsSettings(t, []model.Client{client}))
	if err := clientSvc.SyncInbound(nil, ib.Id, []model.Client{client}); err != nil {
		t.Fatalf("seed linkage: %v", err)
	}
	mkTraffic(t, ib.Id, client.Email, 100, 0, 100, 0, true)

	if err := inboundSvc.DelDepletedClients(ib.Id); err != nil {
		t.Fatalf("DelDepletedClients: %v", err)
	}
	if _, err := inboundSvc.GetInbound(ib.Id); err != nil {
		t.Fatalf("depleted-client cleanup must not delete the inbound: %v", err)
	}
	fresh, err := inboundSvc.GetInbound(ib.Id)
	if err != nil {
		t.Fatalf("reload inbound: %v", err)
	}
	clients, err := inboundSvc.GetClients(fresh)
	if err != nil {
		t.Fatalf("GetClients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("depleted client should be removed from settings, got %d", len(clients))
	}
	var links int64
	if err := database.GetDB().Model(&model.ClientInbound{}).Where("inbound_id = ?", ib.Id).Count(&links).Error; err != nil {
		t.Fatalf("count links: %v", err)
	}
	if links != 0 {
		t.Fatalf("depleted client association should be removed, got %d links", links)
	}
}

func TestGetClientTrafficByEmailReadsClientsTable(t *testing.T) {
	setupBulkDB(t)
	svc := &ClientService{}
	inboundSvc := &InboundService{}

	source := []model.Client{
		{Email: "alice@x", ID: "11111111-1111-1111-1111-111111111111", SubID: "sa", Enable: true},
	}
	ib := mkInbound(t, 21003, model.VLESS, clientsSettings(t, source))
	if err := svc.SyncInbound(nil, ib.Id, source); err != nil {
		t.Fatalf("seed linkage: %v", err)
	}
	mkTraffic(t, ib.Id, "alice@x", 1, 2, 0, 0, true)

	tr, err := inboundSvc.GetClientTrafficByEmail("alice@x")
	if err != nil {
		t.Fatalf("GetClientTrafficByEmail: %v", err)
	}
	if tr == nil {
		t.Fatalf("expected traffic, got nil")
	}
	if tr.UUID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("UUID not enriched from clients table, got %q", tr.UUID)
	}
	if tr.SubId != "sa" {
		t.Fatalf("SubId not enriched from clients table, got %q", tr.SubId)
	}
}

func TestInboundDepletedCleanupPreservesResellerUsage(t *testing.T) {
	for _, failArchive := range []bool{false, true} {
		t.Run(fmt.Sprintf("archive_failure_%v", failArchive), func(t *testing.T) {
			setupBulkDB(t)
			db := database.GetDB()
			reseller := model.Reseller{Username: "depleted-owner", Password: "hash", Enable: true, TrafficLimit: 1000}
			if err := db.Create(&reseller).Error; err != nil {
				t.Fatal(err)
			}
			client := model.Client{Email: "depleted-owner@example.test", ID: "44444444-4444-4444-4444-444444444444", SubID: "depleted-owner", Enable: true, TotalGB: 100}
			ib := mkInbound(t, 21005, model.VLESS, clientsSettings(t, []model.Client{client}))
			if err := (&ClientService{}).SyncInbound(nil, ib.Id, []model.Client{client}); err != nil {
				t.Fatal(err)
			}
			for _, row := range []any{&model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}, &model.ResellerInbound{ResellerId: reseller.Id, InboundId: ib.Id}} {
				if err := db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			mkTraffic(t, ib.Id, client.Email, 60, 40, 100, 0, true)
			if failArchive {
				if err := db.Exec("CREATE TRIGGER reject_depleted_archive BEFORE INSERT ON reseller_traffic_archives BEGIN SELECT RAISE(ABORT, 'archive failed'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			err := (&InboundService{}).DelDepletedClients(ib.Id)
			if failArchive {
				if err == nil {
					t.Fatal("archive failure allowed deletion")
				}
				var count int64
				if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("traffic lost on rollback: %d %v", count, err)
				}
				if err := db.Model(&model.ClientInbound{}).Where("inbound_id = ?", ib.Id).Count(&count).Error; err != nil || count != 1 {
					t.Fatalf("association lost on rollback: %d %v", count, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var archive model.ResellerTrafficArchive
			if err := db.Where("reseller_id = ? AND email = ?", reseller.Id, client.Email).First(&archive).Error; err != nil || archive.ConsumedBytes != 100 {
				t.Fatalf("incorrect archive: %+v %v", archive, err)
			}
			stat, err := (&ResellerService{}).Stat(&reseller)
			if err != nil || stat.UsedTraffic != 100 || stat.AllocatedTraffic != 100 {
				t.Fatalf("usage changed after deletion: %+v %v", stat, err)
			}
		})
	}
}

func TestDepletedCleanupUsesGlobalCountersAndAvoidsOverflow(t *testing.T) {
	for _, mode := range []string{"client", "inbound"} {
		t.Run(mode, func(t *testing.T) {
			setupBulkDB(t)
			db := database.GetDB()
			clients := []model.Client{
				{Email: "global-cleanup@example.test", ID: "11111111-1111-1111-1111-111111111111", SubID: "global-cleanup", Enable: true, TotalGB: 100},
				{Email: "overflow-cleanup@example.test", ID: "22222222-2222-2222-2222-222222222222", SubID: "overflow-cleanup", Enable: true, TotalGB: math.MaxInt64},
				{Email: "safe-cleanup@example.test", ID: "33333333-3333-3333-3333-333333333333", SubID: "safe-cleanup", Enable: true, TotalGB: 100},
			}
			inbound := mkInbound(t, 21006, model.VLESS, clientsSettings(t, clients))
			clientSvc := &ClientService{}
			inboundSvc := &InboundService{}
			if err := clientSvc.SyncInbound(nil, inbound.Id, clients); err != nil {
				t.Fatal(err)
			}
			mkTraffic(t, inbound.Id, clients[0].Email, 60, 10, 100, 0, true)
			mkTraffic(t, inbound.Id, clients[1].Email, math.MaxInt64-5, 10, math.MaxInt64, 0, true)
			mkTraffic(t, inbound.Id, clients[2].Email, 60, 39, 100, 0, true)
			if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "master", Email: clients[0].Email, Up: 20, Down: 40}).Error; err != nil {
				t.Fatal(err)
			}
			if mode == "client" {
				count, _, err := clientSvc.DelDepleted(inboundSvc)
				if err != nil || count != 2 {
					t.Fatalf("client cleanup = %d %v", count, err)
				}
			} else if err := inboundSvc.DelDepletedClients(inbound.Id); err != nil {
				t.Fatal(err)
			}
			fresh, err := inboundSvc.GetInbound(inbound.Id)
			if err != nil {
				t.Fatal(err)
			}
			remaining, err := inboundSvc.GetClients(fresh)
			if err != nil || len(remaining) != 1 || remaining[0].Email != clients[2].Email {
				t.Fatalf("remaining = %+v %v", remaining, err)
			}
		})
	}
}

func TestScopedInboundDepletedCleanupDoesNotDeleteUnrelatedOrphanCounters(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	client := model.Client{Email: "target@example.test", ID: "11111111-1111-1111-1111-111111111111", SubID: "target", Enable: true, TotalGB: 100}
	inbound := mkInbound(t, 21007, model.VLESS, clientsSettings(t, []model.Client{client}))
	if err := (&ClientService{}).SyncInbound(nil, inbound.Id, []model.Client{client}); err != nil {
		t.Fatal(err)
	}
	mkTraffic(t, inbound.Id, client.Email, 100, 0, 100, 0, true)
	// The unrelated orphan is depleted and has no client_inbounds row. Its
	// counters must not be deleted by a request targeting a different inbound.
	mkTraffic(t, inbound.Id+1000, "orphan@example.test", 45, 55, 100, 0, true)
	if err := (&InboundService{}).DelDepletedClients(inbound.Id); err != nil {
		t.Fatal(err)
	}
	var rows []xray.ClientTraffic
	if err := db.Order("email").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Email != "orphan@example.test" || rows[0].Up != 45 || rows[0].Down != 55 {
		t.Fatalf("scoped cleanup changed unrelated traffic: %+v", rows)
	}
}
