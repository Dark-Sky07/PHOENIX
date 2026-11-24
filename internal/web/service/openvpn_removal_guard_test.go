package service

import (
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestOpenVPNRemovalRetainsAccountingEvidence(t *testing.T) {
	ib, client, reseller := seedPPPRemovalGuard(t)
	db := database.GetDB()
	if err := db.Model(ib).Updates(map[string]any{"protocol": model.OpenVPN, "enable": false}).Error; err != nil {
		t.Fatal(err)
	}
	svc, inbound := &ClientService{}, &InboundService{}
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"delete", func() error { _, e := svc.Delete(inbound, client.Id, false); return e }},
		{"email", func() error { _, e := svc.DeleteByEmail(inbound, client.Email, false); return e }},
		{"scoped", func() error {
			_, e := svc.DeleteByEmailForInbounds(inbound, client.Email, false, []int{ib.Id})
			return e
		}},
		{"detach", func() error { _, e := svc.Detach(inbound, client.Id, []int{ib.Id}); return e }},
		{"inbound client", func() error { _, e := svc.DelInboundClientByEmail(inbound, ib.Id, client.Email, false); return e }},
		{"bulk", func() error { _, _, e := svc.BulkDelete(inbound, []string{client.Email}, false); return e }},
		{"unassign", func() error { return (&ResellerService{}).UnassignClient(reseller.Id, client.Email) }},
		{"reseller", func() error { return (&ResellerService{}).Delete(reseller.Id) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			if e := c.run(); e == nil || !strings.Contains(e.Error(), "OpenVPN") {
				t.Fatalf("unsafe removal: %v", e)
			}
		})
	}
	for _, item := range []struct {
		row   any
		where string
		arg   any
	}{
		{&model.ClientRecord{}, "id = ?", client.Id}, {&model.Reseller{}, "id = ?", reseller.Id},
		{&model.ResellerClient{}, "reseller_id = ?", reseller.Id}, {&model.ClientInbound{}, "client_id = ?", client.Id},
		{&xray.ClientTraffic{}, "email = ?", client.Email},
	} {
		var count int64
		if err := db.Model(item.row).Where(item.where, item.arg).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("evidence changed %T: %d %v", item.row, count, err)
		}
	}
	if err := db.Where("client_id = ?", client.Id).Delete(&model.ClientInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenDaemonRemoval(db, []string{"  ALICE@example.test\t"}, nil); err == nil {
		t.Fatal("settings-only identity was not protected")
	}
	if err := rejectUnprovenDaemonRemoval(db, []string{"other@example.test"}, nil); err != nil {
		t.Fatalf("unrelated client blocked: %v", err)
	}
}
