package service

import (
	"math"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestPeriodicDaemonClientsRejectExhaustedAndCorruptUsage(t *testing.T) {
	past := time.Now().Add(-time.Hour).UnixMilli()
	for _, tc := range []struct {
		name                                                                                     string
		up, down, total, globalUp, globalDown, clientTotal, expiry, clientExpiry, activateBefore int64
		allowed                                                                                  bool
	}{
		{name: "healthy", up: 30, down: 20, total: 100, allowed: true},
		{name: "exact local quota", up: 80, down: 20, total: 100},
		{name: "exact canonical quota", up: 80, down: 20, total: 0, clientTotal: 100},
		{name: "global directional max", up: 10, down: 10, total: 100, globalUp: 90},
		{name: "local negative unlimited", up: -1, total: 0},
		{name: "global negative unlimited", globalDown: -1},
		{name: "overflow", up: math.MaxInt64, down: 4, total: math.MaxInt64},
		{name: "row expiry", expiry: past},
		{name: "canonical expiry", clientExpiry: past},
		{name: "start on first use", expiry: -1000, allowed: true},
		{name: "missed activation deadline", clientExpiry: -1000, activateBefore: past},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			row := xray.ClientTraffic{Email: "alice@example.test", Enable: true, Up: tc.up, Down: tc.down, Total: tc.total, ExpiryTime: tc.expiry}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			if tc.globalUp != 0 || tc.globalDown != 0 {
				// SQL TRIM does not remove tab/newline; the Go-normalized snapshot still matters.
				if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "master", Email: "\tALICE@example.test\n", Up: tc.globalUp, Down: tc.globalDown}).Error; err != nil {
					t.Fatal(err)
				}
			}
			client := model.Client{Email: "alice@example.test", Enable: true, TotalGB: tc.clientTotal, ExpiryTime: tc.clientExpiry, ActivateBefore: tc.activateBefore}
			got, err := (&InboundService{}).FilterDaemonAccountingClients([]model.Client{client})
			if err != nil || (len(got) == 1) != tc.allowed || got == nil {
				t.Fatalf("got %v err=%v allowed=%v", got, err, tc.allowed)
			}
		})
	}
}

func TestPeriodicDaemonQuotaFilterDefersOnGlobalSnapshotQueryFailure(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	if err := db.Create(&xray.ClientTraffic{Email: "a@test", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&model.ClientGlobalTraffic{}); err != nil {
		t.Fatal(err)
	}
	got, err := (&InboundService{}).FilterDaemonAccountingClients([]model.Client{{Email: "a@test", Enable: true}})
	if err == nil || got != nil {
		t.Fatalf("missing upstream evidence accepted: %v %v", got, err)
	}
}
