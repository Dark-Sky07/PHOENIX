package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
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

func TestPPPInboundEditRejectsUnprovenClientRemoval(t *testing.T) {
	for _, protocol := range []model.Protocol{model.PPTP, model.SSTP, model.L2TP} {
		t.Run(string(protocol), func(t *testing.T) {
			ib, client, _ := seedPPPRemovalGuard(t)
			db := database.GetDB()
			ib.Protocol = protocol
			if err := db.Model(ib).Update("protocol", protocol).Error; err != nil {
				t.Fatal(err)
			}
			updated := *ib
			updated.Settings = `{"clients":[]}`
			err := db.Transaction(func(tx *gorm.DB) error {
				return (&InboundService{}).updateClientTraffics(tx, ib, &updated)
			})
			if err == nil || !strings.Contains(err.Error(), "PPP") {
				t.Fatalf("unproven PPP client removal accepted: %v", err)
			}
			var count int64
			if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("traffic evidence removed: %d %v", count, err)
			}
			if err := db.Model(&model.ClientInbound{}).Where("inbound_id = ? AND client_id = ?", ib.Id, client.Id).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("client link removed: %d %v", count, err)
			}
		})
	}
}

func TestPPPBlankSettingsPreserveClientListBeforeNormalization(t *testing.T) {
	inbound := &model.Inbound{Protocol: model.PPTP, Settings: ""}
	preserveDaemonInboundClients(inbound, `{"clients":[{"email":"keep@example.test"}]}`)
	if !strings.Contains(inbound.Settings, "keep@example.test") {
		t.Fatalf("blank settings lost client list: %s", inbound.Settings)
	}
}

func TestPPPInboundEditOmittingClientsPreservesAttachments(t *testing.T) {
	ib, client, _ := seedPPPRemovalGuard(t)
	db := database.GetDB()
	var old model.Inbound
	if err := db.First(&old, ib.Id).Error; err != nil {
		t.Fatal(err)
	}
	updated := old
	updated.Settings = `{}`
	if _, _, err := (&InboundService{}).UpdateInbound(&updated); err != nil {
		t.Fatalf("edit without client list failed: %v", err)
	}
	var saved model.Inbound
	if err := db.First(&saved, ib.Id).Error; err != nil || !strings.Contains(saved.Settings, client.Email) {
		t.Fatalf("omitted client list was discarded: %+v %v", saved, err)
	}
	var count int64
	if err := db.Model(&model.ClientInbound{}).Where("inbound_id = ? AND client_id = ?", ib.Id, client.Id).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("client link removed: %d %v", count, err)
	}
}

func TestLocalPPPInboundDeletionRetainsIdentityAndLinks(t *testing.T) {
	for _, protocol := range []model.Protocol{model.PPTP, model.SSTP, model.L2TP} {
		t.Run(string(protocol), func(t *testing.T) {
			ib, client, _ := seedPPPRemovalGuard(t)
			db := database.GetDB()
			if err := db.Model(ib).Updates(map[string]any{"protocol": protocol, "enable": false}).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := (&InboundService{}).DelInbound(ib.Id); err == nil || !strings.Contains(err.Error(), "verified disconnect") {
				t.Fatalf("unverified local PPP inbound removal accepted: %v", err)
			}
			var saved model.Inbound
			if err := db.First(&saved, ib.Id).Error; err != nil || saved.Settings != ib.Settings {
				t.Fatalf("inbound identity changed: %+v %v", saved, err)
			}
			var count int64
			if err := db.Model(&model.ClientInbound{}).Where("inbound_id = ? AND client_id = ?", ib.Id, client.Id).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("client link removed: %d %v", count, err)
			}
		})
	}
}

func TestLocalPPPProtocolConversionDoesNotBypassRemovalGuard(t *testing.T) {
	for _, protocol := range []model.Protocol{model.PPTP, model.SSTP, model.L2TP} {
		t.Run(string(protocol), func(t *testing.T) {
			ib, client, _ := seedPPPRemovalGuard(t)
			db := database.GetDB()
			if err := db.Model(ib).Updates(map[string]any{"protocol": protocol, "enable": false}).Error; err != nil {
				t.Fatal(err)
			}
			var original model.Inbound
			if err := db.First(&original, ib.Id).Error; err != nil {
				t.Fatal(err)
			}
			updated := original
			updated.Protocol = model.VLESS
			if _, _, err := (&InboundService{}).UpdateInbound(&updated); err == nil || !strings.Contains(err.Error(), "verified disconnect") {
				t.Fatalf("PPP conversion accepted: %v", err)
			}
			var saved model.Inbound
			if err := db.First(&saved, ib.Id).Error; err != nil || saved.Protocol != protocol || saved.Settings != original.Settings {
				t.Fatalf("inbound mutated before rejection: %+v %v", saved, err)
			}
			var count int64
			if err := db.Model(&model.ClientInbound{}).Where("inbound_id = ? AND client_id = ?", ib.Id, client.Id).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("client link removed: %d %v", count, err)
			}
		})
	}
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

func TestL2TPRemovalAndUnassignmentStayBlockedBeforeMutation(t *testing.T) {
	ib, client, reseller := seedPPPRemovalGuard(t)
	db := database.GetDB()
	if err := db.Model(ib).Update("protocol", model.L2TP).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ClientService{}
	inboundSvc := &InboundService{}
	for _, check := range []struct {
		name string
		run  func() error
	}{
		{"delete", func() error { _, err := svc.Delete(inboundSvc, client.Id, false); return err }},
		{"delete by email", func() error { _, err := svc.DeleteByEmail(inboundSvc, client.Email, false); return err }},
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
				t.Fatalf("unsafe L2TP removal allowed: %v", err)
			}
		})
	}
	for _, item := range []struct {
		obj   any
		where string
		arg   any
	}{
		{&model.ClientRecord{}, "id = ?", client.Id}, {&model.Reseller{}, "id = ?", reseller.Id}, {&model.ResellerClient{}, "reseller_id = ?", reseller.Id}, {&model.ClientInbound{}, "client_id = ?", client.Id}, {&xray.ClientTraffic{}, "email = ?", client.Email},
	} {
		var count int64
		if err := db.Model(item.obj).Where(item.where, item.arg).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("L2TP evidence changed for %T: %d %v", item.obj, count, err)
		}
	}
	var saved model.Inbound
	if err := db.First(&saved, ib.Id).Error; err != nil || saved.Settings != ib.Settings {
		t.Fatalf("L2TP inbound altered: %v", err)
	}
	// A missing join row does not make a retained settings credential safe.
	if err := db.Where("client_id = ?", client.Id).Delete(&model.ClientInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(ib).Update("enable", false).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{client.Email}, nil); err == nil {
		t.Fatal("disabled L2TP settings-only attachment accepted")
	}
	if err := rejectUnprovenPPPRemoval(db, []string{"other@example.test"}, nil); err != nil {
		t.Fatalf("unrelated client blocked: %v", err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{client.Email}, []int{}); err != nil {
		t.Fatalf("empty scope blocked: %v", err)
	}
}

func TestPPPRemovalGuardRejectsUnresolvedAttachments(t *testing.T) {
	for _, protocol := range []model.Protocol{model.PPTP, model.SSTP, model.L2TP} {
		for _, condition := range []string{"orphan-link", "blank-linked-email", "blank-settings-email", "null-settings-client"} {
			t.Run(string(protocol)+"/"+condition, func(t *testing.T) {
				ib, client, _ := seedPPPRemovalGuard(t)
				db := database.GetDB()
				if err := db.Model(ib).Updates(map[string]any{"protocol": protocol, "settings": `{"clients":[]}`}).Error; err != nil {
					t.Fatal(err)
				}
				switch condition {
				case "orphan-link":
					if err := db.Delete(client).Error; err != nil {
						t.Fatal(err)
					}
				case "blank-linked-email":
					if err := db.Model(client).Update("email", " \t ").Error; err != nil {
						t.Fatal(err)
					}
				case "blank-settings-email", "null-settings-client":
					if err := db.Where("client_id = ?", client.Id).Delete(&model.ClientInbound{}).Error; err != nil {
						t.Fatal(err)
					}
					settings := `{"clients":[{"email":" "}]}`
					if condition == "null-settings-client" {
						settings = `{"clients":[null]}`
					}
					if err := db.Model(ib).Update("settings", settings).Error; err != nil {
						t.Fatal(err)
					}
				}
				if err := rejectUnprovenPPPRemoval(db, []string{"unrelated@example.test"}, []int{ib.Id}); err == nil || !strings.Contains(err.Error(), "manual reconciliation") {
					t.Fatalf("unresolved attachment accepted: %v", err)
				}
				if err := rejectUnprovenPPPRemoval(db, []string{"unrelated@example.test"}, []int{}); err != nil {
					t.Fatalf("empty scope blocked: %v", err)
				}
			})
		}
	}
}

func TestPPPRemovalGuardFindsGoNormalizedLinkedIdentity(t *testing.T) {
	for _, protocol := range []model.Protocol{model.PPTP, model.SSTP, model.L2TP} {
		for _, pair := range [][2]string{{"\tALICE@example.test\n", "alice@example.test"}, {"ÜSER@example.test", "üser@example.test"}} {
			t.Run(string(protocol)+pair[1], func(t *testing.T) {
				ib, client, _ := seedPPPRemovalGuard(t)
				db := database.GetDB()
				if err := db.Model(ib).Updates(map[string]any{"protocol": protocol, "settings": `{"clients":[]}`}).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(client).Update("email", pair[0]).Error; err != nil {
					t.Fatal(err)
				}
				if err := rejectUnprovenPPPRemoval(db, []string{pair[1]}, []int{ib.Id}); err == nil {
					t.Fatal("linked Go-normalized identity allowed unsafe removal")
				}
				if err := rejectUnprovenPPPRemoval(db, []string{"unrelated@example.test"}, []int{ib.Id}); err != nil {
					t.Fatalf("unrelated identity blocked: %v", err)
				}
			})
		}
	}
}

func TestL2TPRemovalGuardCatchesOrphanSessionMarker(t *testing.T) {
	ib, client, _ := seedPPPRemovalGuard(t)
	db := database.GetDB()
	if err := db.Model(ib).Updates(map[string]any{"protocol": model.L2TP, "settings": `{"clients":[]}`}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("client_id = ?", client.Id).Delete(&model.ClientInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("XUI_BIN_FOLDER"), "l2tp", "7", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ppp0")
	if err := os.WriteFile(path, []byte("ALICE@example.test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{client.Email}, nil); err == nil || !strings.Contains(err.Error(), "L2TP") {
		t.Fatalf("orphan L2TP marker ignored: %v", err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{"other@example.test"}, nil); err != nil {
		t.Fatalf("unrelated marker blocked: %v", err)
	}
	if err := os.WriteFile(path, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(db, []string{"other@example.test"}, nil); err == nil {
		t.Fatal("malformed L2TP marker ignored")
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
