package service

import (
	"math"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

func TestClientStatEditCannotEnableExhaustedOrCorruptUsage(t *testing.T) {
	for _, tc := range []struct {
		name          string
		up, down      int64
		globalUp      int64
		total, expiry int64
		wantError     bool
	}{
		{"exact quota", 80, 20, 0, 100, 0, true},
		{"global quota", 10, 10, 90, 100, 0, true},
		{"overflow", math.MaxInt64, 3, 0, math.MaxInt64, 0, true},
		{"negative global", 10, 10, -1, 0, 0, true},
		{"expired", 10, 10, 0, 100, time.Now().Add(-time.Hour).UnixMilli(), true},
		{"increased quota", 80, 20, 0, 101, 0, false},
		{"unlimited valid", 80, 20, 0, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			row := xray.ClientTraffic{Email: "quota-edit@example.test", Up: tc.up, Down: tc.down, Enable: false}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			if tc.globalUp != 0 {
				if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "master", Email: row.Email, Up: tc.globalUp}).Error; err != nil {
					t.Fatal(err)
				}
			}
			client := model.Client{Email: row.Email, Enable: true, TotalGB: tc.total, ExpiryTime: tc.expiry}
			err := db.Transaction(func(tx *gorm.DB) error { return (&InboundService{}).UpdateClientStat(tx, row.Email, &client) })
			if (err != nil) != tc.wantError {
				t.Fatalf("edit error=%v wantError=%v", err, tc.wantError)
			}
			if err := db.First(&row, row.Id).Error; err != nil {
				t.Fatal(err)
			}
			if row.Enable == tc.wantError || row.Up != tc.up || row.Down != tc.down {
				t.Fatalf("unexpected traffic: %+v", row)
			}
		})
	}
}

func TestInboundEditDoesNotReactivateExhaustedClient(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Where("email = ?", client.Email).Delete(&model.ResellerClient{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&xray.ClientTraffic{}).Where("email = ?", client.Email).Updates(map[string]any{"up": 500, "down": 1, "enable": false}).Error; err != nil {
		t.Fatal(err)
	}
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	var old model.Inbound
	if err := db.First(&old, link.InboundId).Error; err != nil {
		t.Fatal(err)
	}
	updated := old
	updated.Remark = "unrelated edit"
	if _, _, err := (&InboundService{}).UpdateInbound(&updated); err == nil {
		t.Fatal("edit enabled exhausted client")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil || row.Enable {
		t.Fatalf("client enabled: %+v %v", row, err)
	}
	var saved model.Inbound
	if err := db.First(&saved, old.Id).Error; err != nil || saved.Remark != old.Remark {
		t.Fatalf("failed edit committed: %+v %v", saved, err)
	}
}
