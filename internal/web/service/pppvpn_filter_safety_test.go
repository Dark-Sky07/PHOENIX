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
			if err := db.Create(&xray.ClientTraffic{InboundId: ib.Id, Email: "active@example.test", Enable: true}).Error; err != nil {
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

func TestDaemonFilterOmitsMissingOrAmbiguousTrafficIdentities(t *testing.T) {
	for _, protocol := range []model.Protocol{model.TUIC, model.OpenVPN, model.L2TP, model.SSTP, model.PPTP} {
		t.Run(string(protocol), func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			ib := model.Inbound{Id: 123, Protocol: protocol, Settings: `{"clients":[{"email":"ok@example.test","enable":true},{"email":"missing@example.test","enable":true},{"email":"ambiguous@example.test","enable":true}]}`}
			for _, row := range []xray.ClientTraffic{
				{Email: "ok@example.test", Enable: true},
				{Email: "ambiguous@example.test", Enable: true},
				{Email: "AMBIGUOUS@example.test", Enable: false},
			} {
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
			filtered, err := (&InboundService{}).buildRuntimeInboundForAPI(db, &ib)
			if err != nil {
				t.Fatal(err)
			}
			var settings struct {
				Clients []struct {
					Email string `json:"email"`
				} `json:"clients"`
			}
			if err := json.Unmarshal([]byte(filtered.Settings), &settings); err != nil {
				t.Fatal(err)
			}
			if len(settings.Clients) != 1 || settings.Clients[0].Email != "ok@example.test" {
				t.Fatalf("unsafe clients survived filter: %+v", settings.Clients)
			}
		})
	}
}

func TestDaemonFilterDetectsGoNormalizedCrossInboundAmbiguity(t *testing.T) {
	for _, protocol := range []model.Protocol{model.TUIC, model.OpenVPN, model.L2TP, model.SSTP, model.PPTP} {
		t.Run(string(protocol), func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			ib := model.Inbound{Id: 123, Enable: true, Protocol: protocol, Settings: `{"clients":[{"email":"tab@example.test","enable":true},{"email":"üser@example.test","enable":true},{"email":"onlytab@example.test","enable":true},{"email":"öonly@example.test","enable":true},{"email":"safe@example.test","enable":true}]}`}
			if err := db.Create(&ib).Error; err != nil {
				t.Fatal(err)
			}
			for _, row := range []xray.ClientTraffic{
				{InboundId: ib.Id, Email: "tab@example.test", Enable: true},
				{InboundId: 999, Email: "\tTAB@example.test\n", Enable: false},
				{InboundId: ib.Id, Email: "üser@example.test", Enable: true},
				{InboundId: 999, Email: "ÜSER@example.test", Enable: false},
				{InboundId: ib.Id, Email: "\tonlytab@example.test\n", Enable: true},
				{InboundId: ib.Id, Email: "ÖONLY@example.test", Enable: true},
				{InboundId: ib.Id, Email: "safe@example.test", Enable: true},
			} {
				if err := db.Create(&row).Error; err != nil {
					t.Fatal(err)
				}
			}
			filtered, err := (&InboundService{}).buildRuntimeInboundForAPI(db, &ib)
			if err != nil {
				t.Fatal(err)
			}
			var settings struct {
				Clients []struct {
					Email string `json:"email"`
				} `json:"clients"`
			}
			if err := json.Unmarshal([]byte(filtered.Settings), &settings); err != nil {
				t.Fatal(err)
			}
			if len(settings.Clients) != 1 || settings.Clients[0].Email != "safe@example.test" {
				t.Fatalf("Go-normalized ambiguous credentials were retained: %+v", settings.Clients)
			}
			if status := readCounterIncidentStatus(db); !status.Available || status.DaemonIdentityCount != 4 {
				t.Fatalf("dashboard did not report ambiguous and SQL-unresolvable identities: %+v", status)
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
