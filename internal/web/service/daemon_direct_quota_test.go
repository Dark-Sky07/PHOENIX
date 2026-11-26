package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestDirectDaemonRuntimeAppliesQuotaAndIdentityPerCredential(t *testing.T) {
	for _, protocol := range []model.Protocol{model.TUIC, model.OpenVPN, model.L2TP, model.SSTP, model.PPTP} {
		t.Run(string(protocol), func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			for _, row := range []xray.ClientTraffic{
				{Email: "safe@test", Enable: true, Up: 10}, {Email: "spent@test", Enable: true, Up: 100, Total: 100},
				{Email: "global@test", Enable: true, Up: 5, Total: 100}, {Email: "negative@test", Enable: true, Up: -1},
				{Email: "big@test", Enable: true, Up: 9007199254740992}, {Email: "dup@test", Enable: true, Up: 100},
				{Email: "null@test", Enable: true}, {Email: "expiry@test", Enable: true}, {Email: "delayed@test", Enable: true},
			} {
				r := row
				if err := db.Create(&r).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "master", Email: "\tGLOBAL@test\n", Up: 100}).Error; err != nil {
				t.Fatal(err)
			}
			past := time.Now().Add(-time.Hour).UnixMilli()
			settings := fmt.Sprintf(`{"clients":[{"email":"safe@test"},{"email":"spent@test","enable":true},{"email":"global@test","enable":true},{"email":"negative@test","enable":true},{"email":"big@test","enable":true,"totalGB":9007199254740993},{"email":"dup@test","enable":true,"password":"bad","totalGB":100},{"email":"dup@test","enable":true,"password":"good"},{"email":"null@test","enable":true,"totalGB":null},{"email":"expiry@test","enable":true,"expiryTime":%d},{"email":"delayed@test","enable":true,"expiryTime":-1000,"activateBefore":%d}]}`, past, past)
			ib := model.Inbound{Protocol: protocol, Settings: settings}
			got, err := (&InboundService{}).buildRuntimeInboundForAPI(db, &ib)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Clients []struct {
					Email     string `json:"email"`
					Password  string `json:"password"`
					TrafficID int    `json:"traffic_id"`
				} `json:"clients"`
			}
			if err := json.Unmarshal([]byte(got.Settings), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.Clients) != 3 || payload.Clients[0].Email != "safe@test" || payload.Clients[1].Email != "big@test" || payload.Clients[2].Password != "good" {
				t.Fatalf("unexpected credentials: %+v", payload.Clients)
			}
			if !strings.Contains(got.Settings, "9007199254740993") {
				t.Fatalf("large quota rounded: %s", got.Settings)
			}
			if protocol == model.TUIC {
				for _, client := range payload.Clients {
					if client.TrafficID <= 0 {
						t.Fatalf("missing TUIC traffic id: %+v", client)
					}
				}
			}
		})
	}
}

func TestDirectDaemonRuntimeDefersOnGlobalEvidenceFailure(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	if err := db.Create(&xray.ClientTraffic{Email: "a@test", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&model.ClientGlobalTraffic{}); err != nil {
		t.Fatal(err)
	}
	for _, protocol := range []model.Protocol{model.TUIC, model.OpenVPN, model.L2TP, model.SSTP, model.PPTP} {
		got, err := (&InboundService{}).buildRuntimeInboundForAPI(db, &model.Inbound{Protocol: protocol, Settings: `{"clients":[{"email":"a@test","enable":true}]}`})
		if err == nil || got != nil {
			t.Fatalf("%s applied raw settings on DB error: %v %v", protocol, got, err)
		}
	}
}

func TestDirectDaemonQuotaReadsCallerTransaction(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	row := xray.ClientTraffic{Email: "transaction@test", Enable: true, Up: 101, Total: 100}
	if err := tx.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	ib := model.Inbound{Protocol: model.TUIC, Settings: `{"clients":[{"email":"transaction@test","enable":true}]}`}
	got, err := (&InboundService{}).buildRuntimeInboundForAPI(tx, &ib)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Clients []json.RawMessage `json:"clients"`
	}
	if err := json.Unmarshal([]byte(got.Settings), &payload); err != nil || len(payload.Clients) != 0 {
		t.Fatalf("uncommitted exhausted row ignored: %s %v", got.Settings, err)
	}
}

func TestDirectDaemonRejectsInvalidClientList(t *testing.T) {
	setupConflictDB(t)
	for _, list := range []string{"null", "{}", `"raw"`} {
		_, err := (&InboundService{}).buildRuntimeInboundForAPI(database.GetDB(), &model.Inbound{Protocol: model.PPTP, Settings: `{"clients":` + list + `}`})
		if err == nil {
			t.Fatalf("invalid list %s accepted", list)
		}
	}
}

func TestDirectDaemonRespectsCanonicalClientEvidence(t *testing.T) {
	past := time.Now().Add(-time.Hour).UnixMilli()
	for _, tc := range []struct {
		name                      string
		enabled                   bool
		total, expiry, activation int64
		ambiguous, want           bool
	}{
		{name: "healthy", enabled: true, total: 200, want: true},
		{name: "disabled", enabled: false},
		{name: "exhausted", enabled: true, total: 100},
		{name: "expired", enabled: true, expiry: past},
		{name: "activation deadline", enabled: true, expiry: -1000, activation: past},
		{name: "ambiguous", enabled: true, ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			if err := db.Create(&xray.ClientTraffic{Email: "alice@test", Enable: true, Up: 100}).Error; err != nil {
				t.Fatal(err)
			}
			c := model.ClientRecord{Email: "alice@test", Enable: true, TotalGB: tc.total, ExpiryTime: tc.expiry, ActivateBefore: tc.activation}
			if err := db.Create(&c).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&c).Update("enable", tc.enabled).Error; err != nil {
				t.Fatal(err)
			}
			if tc.ambiguous {
				if err := db.Create(&model.ClientRecord{Email: "\tALICE@test\n", Enable: true}).Error; err != nil {
					t.Fatal(err)
				}
			}
			got, err := (&InboundService{}).buildRuntimeInboundForAPI(db, &model.Inbound{Protocol: model.OpenVPN, Settings: `{"clients":[{"email":"alice@test","enable":true}]}`})
			if err != nil {
				t.Fatal(err)
			}
			var p struct {
				Clients []any `json:"clients"`
			}
			if err := json.Unmarshal([]byte(got.Settings), &p); err != nil {
				t.Fatal(err)
			}
			if (len(p.Clients) == 1) != tc.want {
				t.Fatalf("unexpected authorization: %s", got.Settings)
			}
		})
	}
}
