package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func seedPPPRemovalGuard(t *testing.T) (*model.Inbound, *model.ClientRecord, *model.Reseller) {
	t.Helper()
	setupBulkDB(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	db := database.GetDB()
	ib := mkInbound(t, 32133, model.PPTP, `{"clients":[{"email":"alice@example.test","password":"secret","enable":true}]}`)
	client := &model.ClientRecord{Email: "alice@example.test", Enable: true}
	reseller := &model.Reseller{Username: "ppp-removal", Password: "hash", Enable: true, TrafficLimit: 1000}
	for _, row := range []any{client, reseller, &xray.ClientTraffic{Email: client.Email, Up: 100, Down: 50, Enable: true}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []any{&model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}, &model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	return ib, client, reseller
}

func TestPPPRemovalAndUnassignmentStayBlockedBeforeMutation(t *testing.T) {
	ib, client, reseller := seedPPPRemovalGuard(t)
	db := database.GetDB()
	svc := &ClientService{}
	inboundSvc := &InboundService{}
	for _, check := range []struct {
		name string
		run  func() error
	}{
		{"delete", func() error { _, err := svc.Delete(inboundSvc, client.Id, false); return err }},
		{"delete email", func() error { _, err := svc.DeleteByEmail(inboundSvc, client.Email, false); return err }},
		{"scoped delete", func() error {
			_, err := svc.DeleteByEmailForInbounds(inboundSvc, client.Email, false, []int{ib.Id})
			return err
		}},
		{"detach", func() error { _, err := svc.Detach(inboundSvc, client.Id, []int{ib.Id}); return err }},
		{"inbound delete", func() error {
			_, err := svc.DelInboundClientByEmail(inboundSvc, ib.Id, client.Email, false)
			return err
		}},
		{"bulk delete", func() error { _, _, err := svc.BulkDelete(inboundSvc, []string{client.Email}, false); return err }},
		{"unassign", func() error { return (&ResellerService{}).UnassignClient(reseller.Id, client.Email) }},
		{"delete reseller", func() error { return (&ResellerService{}).Delete(reseller.Id) }},
	} {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); err == nil || !strings.Contains(err.Error(), "PPP") {
				t.Fatalf("unsafe removal allowed: %v", err)
			}
		})
	}
	var count int64
	for _, item := range []struct {
		obj   any
		where string
		arg   any
	}{
		{&model.ClientRecord{}, "id = ?", client.Id}, {&model.Reseller{}, "id = ?", reseller.Id}, {&model.ResellerClient{}, "reseller_id = ?", reseller.Id}, {&model.ClientInbound{}, "client_id = ?", client.Id}, {&xray.ClientTraffic{}, "email = ?", client.Email},
	} {
		if err := db.Model(item.obj).Where(item.where, item.arg).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("evidence changed for %T: %d %v", item.obj, count, err)
		}
	}
	var saved model.Inbound
	if err := db.First(&saved, ib.Id).Error; err != nil || saved.Settings != ib.Settings {
		t.Fatalf("inbound altered: %v", err)
	}
}

func TestPPPRemovalGuardCatchesSettingsDriftAndSpool(t *testing.T) {
	ib, client, _ := seedPPPRemovalGuard(t)
	db := database.GetDB()
	if err := db.Where("client_id = ?", client.Id).Delete(&model.ClientInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{client.Email}, nil); err == nil {
		t.Fatal("settings-only identity ignored")
	}
	if err := db.Model(ib).Update("settings", `{"clients":[]}`).Error; err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("XUI_BIN_FOLDER"), "pppvpn", "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ppp0.closed.session")
	if err := os.WriteFile(path, []byte("closed\tppp0\tALICE@example.test\t12\t34\tsession\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{client.Email}, nil); err == nil {
		t.Fatal("orphan spool ignored")
	}
	if err := rejectUnprovenPPPRemoval(db, []string{"other@example.test"}, nil); err != nil {
		t.Fatalf("unrelated client blocked by valid spool: %v", err)
	}
	if err := os.WriteFile(path, []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{"other@example.test"}, nil); err == nil {
		t.Fatal("unknown spool silently ignored")
	}
}

func TestPPPRemovalScopeIsBoundedAndDisabledStillBlocked(t *testing.T) {
	ib, client, _ := seedPPPRemovalGuard(t)
	db := database.GetDB()
	if err := db.Model(ib).Update("enable", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{client.Email}, nil); err == nil {
		t.Fatal("disabled daemon assumed drained")
	}
	if err := rejectUnprovenPPPRemoval(db, []string{client.Email}, []int{ib.Id + 100}); err != nil {
		t.Fatalf("unrelated scope blocked: %v", err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{client.Email}, []int{}); err != nil {
		t.Fatalf("empty scope blocked: %v", err)
	}
}

func TestPPPBulkPreflightPreventsPartialDeletion(t *testing.T) {
	_, client, _ := seedPPPRemovalGuard(t)
	db := database.GetDB()
	other := &model.ClientRecord{Email: "other@example.test", Enable: true}
	if err := db.Create(other).Error; err != nil {
		t.Fatal(err)
	}
	result, _, err := (&ClientService{}).BulkDelete(&InboundService{}, []string{other.Email, client.Email}, false)
	if err == nil || result.Deleted != 0 {
		t.Fatalf("bulk preflight did not stop partial deletion: %+v %v", result, err)
	}
	var count int64
	if err := db.Model(&model.ClientRecord{}).Where("id IN ?", []int{other.Id, client.Id}).Count(&count).Error; err != nil || count != 2 {
		t.Fatalf("bulk mutated records: %d %v", count, err)
	}
}

func TestPPPLowerLevelBulkAndDetachedRuntimeRemovalBlocked(t *testing.T) {
	ib, client, _ := seedPPPRemovalGuard(t)
	svc := &ClientService{}
	if _, err := svc.delInboundClients(&InboundService{}, ib.Id, []*model.ClientRecord{client}, true); err == nil {
		t.Fatal("lower bulk detachment bypassed PPP guard")
	}
	result := svc.bulkDelInboundClients(&InboundService{}, ib.Id, []string{client.Email}, map[string]*model.ClientRecord{transferEmailKey(client.Email): client}, true)
	if result.perEmailSkipped[transferEmailKey(client.Email)] == "" {
		t.Fatal("optimized bulk detachment bypassed PPP guard")
	}
	var saved model.Inbound
	if err := database.GetDB().First(&saved, ib.Id).Error; err != nil || saved.Settings != ib.Settings {
		t.Fatalf("settings changed: %v", err)
	}
}
