package service

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestGetXrayConfigDefersQuotaEnforcementDuringUpdateMaintenance(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	const email = "update-maintenance@example.test"

	inbound := model.Inbound{
		Tag:      "update-maintenance-vless",
		Enable:   true,
		Port:     29431,
		Protocol: model.VLESS,
		Settings: `{"clients":[{"email":"update-maintenance@example.test","id":"7c64e132-6a7f-4f52-9e13-5f3ec7d8f7df","enable":true}]}`,
	}
	client := model.ClientRecord{
		Email:     email,
		UUID:      "7c64e132-6a7f-4f52-9e13-5f3ec7d8f7df",
		TotalGB:   100,
		Enable:    true,
		ExpiryTime: time.Now().Add(-time.Hour).UnixMilli(),
	}
	for _, row := range []any{&inbound, &client} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{
		InboundId:  inbound.Id,
		Email:      email,
		Enable:     true,
		Total:      100,
		Up:         100,
		ExpiryTime: client.ExpiryTime,
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(updateMaintenanceMarkerPath(), []byte("transactional update\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The shared accounting path protects sidecars and remote-node collectors
	// too; config generation below independently proves the startup path keeps
	// the user in the replacement Xray config.
	if _, _, err := (&InboundService{}).AddTraffic(nil, nil); err != nil {
		t.Fatalf("enforce traffic during maintenance: %v", err)
	}
	assertConfigTrafficClientStillEnabled(t, email)
	if cfg, err := (&XrayService{}).GetXrayConfig(); err != nil {
		t.Fatalf("generate config during maintenance: %v", err)
	} else if !xrayConfigHasClient(cfg, inbound.Tag, email) {
		t.Fatal("maintenance config omitted an otherwise enabled expired/depleted client")
	}
	assertConfigTrafficClientStillEnabled(t, email)

	if err := os.Remove(updateMaintenanceMarkerPath()); err != nil {
		t.Fatal(err)
	}
	if cfg, err := (&XrayService{}).GetXrayConfig(); err != nil {
		t.Fatalf("generate config after maintenance: %v", err)
	} else if xrayConfigHasClient(cfg, inbound.Tag, email) {
		t.Fatal("quota enforcement did not resume after maintenance marker removal")
	}
	var traffic xray.ClientTraffic
	if err := db.Where("email = ?", email).First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if traffic.Enable {
		t.Fatalf("expired/depleted traffic row remained enabled after maintenance: %+v", traffic)
	}
	var record model.ClientRecord
	if err := db.Where("email = ?", email).First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.Enable {
		t.Fatalf("expired/depleted client record remained enabled after maintenance: %+v", record)
	}
}

func assertConfigTrafficClientStillEnabled(t *testing.T, email string) {
	t.Helper()
	var traffic xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", email).First(&traffic).Error; err != nil {
		t.Fatal(err)
	}
	if !traffic.Enable {
		t.Fatalf("maintenance config generation disabled traffic: %+v", traffic)
	}
	var record model.ClientRecord
	if err := database.GetDB().Where("email = ?", email).First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if !record.Enable {
		t.Fatalf("maintenance config generation disabled client record: %+v", record)
	}
}

func xrayConfigHasClient(cfg *xray.Config, inboundTag, email string) bool {
	for _, inbound := range cfg.InboundConfigs {
		if inbound.Tag != inboundTag {
			continue
		}
		var settings struct {
			Clients []struct {
				Email string `json:"email"`
			} `json:"clients"`
		}
		if err := json.Unmarshal(inbound.Settings, &settings); err != nil {
			return false
		}
		for _, client := range settings.Clients {
			if client.Email == email {
				return true
			}
		}
	}
	return false
}
