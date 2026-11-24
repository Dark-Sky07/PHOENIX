package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestOpenvpnProfileRequiresActiveResolvableQuota(t *testing.T) {
	past := time.Now().Add(-time.Hour).UnixMilli()
	for _, tc := range []struct {
		name                                string
		clientEnabled, trafficEnabled       bool
		up, total, expiry                   int64
		pending, ambiguous, globalExhausted bool
		want                                bool
	}{
		{name: "active", clientEnabled: true, trafficEnabled: true, total: 100, want: true},
		{name: "disabled client", trafficEnabled: true, total: 100},
		{name: "disabled traffic", clientEnabled: true, total: 100},
		{name: "exhausted still enabled", clientEnabled: true, trafficEnabled: true, up: 100, total: 100},
		{name: "expired still enabled", clientEnabled: true, trafficEnabled: true, expiry: past},
		{name: "negative unlimited", clientEnabled: true, trafficEnabled: true, up: -1},
		{name: "pending poll", clientEnabled: true, trafficEnabled: true, pending: true},
		{name: "ambiguous identity", clientEnabled: true, trafficEnabled: true, ambiguous: true},
		{name: "exhausted upstream", clientEnabled: true, trafficEnabled: true, total: 100, globalExhausted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupConflictDB(t)
			t.Setenv("XUI_BIN_FOLDER", t.TempDir())
			bin := t.TempDir()
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := os.WriteFile(filepath.Join(bin, "openvpn"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			db := database.GetDB()
			ib := model.Inbound{Tag: "ovpn", Port: 1194, Protocol: model.OpenVPN, Enable: true, Settings: `{"proto":"udp"}`}
			client := model.ClientRecord{Email: "Alice@example.test", Enable: tc.clientEnabled}
			row := xray.ClientTraffic{Email: client.Email, Enable: tc.trafficEnabled, Up: tc.up, Total: tc.total, ExpiryTime: tc.expiry}
			for _, v := range []any{&ib, &client, &row} {
				if err := db.Create(v).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Model(&client).Update("enable", tc.clientEnabled).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&row).Update("enable", tc.trafficEnabled).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}).Error; err != nil {
				t.Fatal(err)
			}
			if tc.ambiguous {
				if err := db.Create(&xray.ClientTraffic{Email: "\tALICE@example.test\n", Enable: true}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.globalExhausted {
				if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "master", Email: "\tALICE@example.test\n", Up: 100}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if tc.pending {
				if err := BeginXrayPoll("pending-profile"); err != nil {
					t.Fatal(err)
				}
			}
			profile, chosen, err := (&InboundService{}).GetOpenvpnProfile("vpn.test", "alice@example.test")
			if tc.want {
				if err != nil || chosen == nil || !strings.Contains(profile, "<cert>") || !strings.Contains(profile, "<key>") {
					t.Fatalf("profile denied: %v", err)
				}
			} else if err == nil || profile != "" || chosen != nil {
				t.Fatalf("unsafe profile issued: %v", err)
			}
		})
	}
}
