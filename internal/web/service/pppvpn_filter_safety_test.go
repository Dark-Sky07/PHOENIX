package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/l2tp"
	"github.com/Dark-Sky07/phoenix/internal/pppvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestPPPFilteredRuntimeOmitsQuotaDisabledCredentials(t *testing.T) {
	for _, protocol := range []model.Protocol{model.L2TP, model.SSTP, model.PPTP} {
		t.Run(string(protocol), func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			cert, key := testTUICCertificate(t)
			clients := []model.Client{{Email: "active@example.test", Password: "one", Enable: true}, {Email: "Disabled@example.test", Password: "two", Enable: true}}
			settings, err := json.Marshal(map[string]any{"psk": "0123456789abcdef0123456789abcdef", "certificatePEM": cert, "privateKeyPEM": key, "clients": clients})
			if err != nil {
				t.Fatal(err)
			}
			ib := model.Inbound{UserId: 1, Tag: "ppp-filter", Port: 21111, Enable: true, Protocol: protocol, Settings: string(settings)}
			if err := db.Create(&ib).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&xray.ClientTraffic{InboundId: ib.Id, Email: "disabled@example.test", Enable: false}).Error; err != nil {
				t.Fatal(err)
			}
			filtered, err := (&InboundService{}).buildRuntimeInboundForAPI(db, &ib)
			if err != nil {
				t.Fatal(err)
			}
			if protocol == model.L2TP {
				instance, ok := l2tp.InstanceFromInbound(filtered, nil)
				if !ok || len(instance.Credentials) != 1 || instance.Credentials[0].Email != "active@example.test" {
					t.Fatalf("unexpected credentials: %+v", instance.Credentials)
				}
			} else {
				instance, ok := pppvpn.InstanceFromInbound(filtered, nil)
				if !ok || len(instance.Credentials) != 1 || instance.Credentials[0].Username != "active@example.test" {
					t.Fatalf("unexpected credentials: %+v", instance.Credentials)
				}
			}
		})
	}
}

func TestPPPReconcileRejectsUnavailableTrafficFilter(t *testing.T) {
	for _, protocol := range []model.Protocol{model.L2TP, model.SSTP, model.PPTP} {
		t.Run(string(protocol), func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			ib := model.Inbound{Id: 101, Protocol: protocol, Enable: true, Settings: `{"clients":[]}`}
			if err := db.Exec("ALTER TABLE client_traffics RENAME TO traffic_unavailable").Error; err != nil {
				t.Fatal(err)
			}
			err := reconcilePPPVPNRuntime(&ib)
			if err == nil || !strings.Contains(err.Error(), "build filtered PPP VPN") {
				t.Fatalf("missing filter failure: %v", err)
			}
		})
	}
}

func TestEnableDaemonRejectsUnavailableTrafficFilterBeforePersistence(t *testing.T) {
	for _, protocol := range []model.Protocol{model.OpenVPN, model.L2TP, model.SSTP, model.PPTP} {
		t.Run(string(protocol), func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			ib := model.Inbound{UserId: 1, Tag: "disabled-daemon", Port: 21112, Protocol: protocol, Settings: `{"clients":[]}`}
			if err := db.Create(&ib).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&ib).Update("enable", false).Error; err != nil {
				t.Fatal(err)
			}
			old := runtime.GetManager()
			runtime.SetManager(runtime.NewManager(runtime.LocalDeps{}))
			t.Cleanup(func() { runtime.SetManager(old) })
			if err := db.Exec("ALTER TABLE client_traffics RENAME TO traffic_unavailable").Error; err != nil {
				t.Fatal(err)
			}
			restart, err := (&InboundService{}).SetInboundEnable(ib.Id, true)
			if err == nil || restart || !strings.Contains(err.Error(), "build filtered daemon") {
				t.Fatalf("unexpected result: restart=%v err=%v", restart, err)
			}
			var persisted model.Inbound
			if err := db.First(&persisted, ib.Id).Error; err != nil || persisted.Enable {
				t.Fatalf("failed enable persisted: %+v %v", persisted, err)
			}
		})
	}
}
