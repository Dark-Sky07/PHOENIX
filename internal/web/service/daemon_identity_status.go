package service

import (
	"encoding/json"
	"fmt"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

// countDaemonIdentityProblems is a local, point-in-time diagnostic of enabled
// inbound configuration, not a session inventory or historical usage proof.
// It deliberately uses the same normalized identity rule as the runtime filter.
func countDaemonIdentityProblems(db *gorm.DB) (int64, error) {
	var inbounds []model.Inbound
	if err := db.Select("id", "settings").Where("node_id IS NULL AND enable = ? AND protocol IN ?", true,
		[]model.Protocol{model.TUIC, model.OpenVPN, model.L2TP, model.SSTP, model.PPTP}).Find(&inbounds).Error; err != nil {
		return 0, err
	}
	var identities []string
	var invalid int64
	for _, inbound := range inbounds {
		var settings map[string]json.RawMessage
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil || settings == nil {
			return 0, fmt.Errorf("cannot inspect daemon inbound %d settings", inbound.Id)
		}
		raw, exists := settings["clients"]
		if !exists || string(raw) == "null" {
			continue
		}
		var clients []json.RawMessage
		if err := json.Unmarshal(raw, &clients); err != nil {
			return 0, fmt.Errorf("cannot inspect daemon inbound %d clients", inbound.Id)
		}
		for _, rawClient := range clients {
			var client map[string]any
			if err := json.Unmarshal(rawClient, &client); err != nil || client == nil {
				invalid++
				continue
			}
			if enabled, ok := client["enable"].(bool); ok && !enabled {
				continue
			}
			email, _ := client["email"].(string)
			key := transferEmailKey(email)
			if key == "" {
				invalid++
				continue
			}
			identities = append(identities, key)
		}
	}
	counts := make(map[string]int, len(identities))
	ids := make(map[string]int, len(identities))
	if len(identities) > 0 {
		// Match the runtime filter's Unicode-aware normalization, not SQL's
		// database-dependent LOWER/TRIM approximation.
		var rows []xray.ClientTraffic
		if err := db.Select("id", "email").Find(&rows).Error; err != nil {
			return 0, err
		}
		for _, row := range rows {
			key := transferEmailKey(row.Email)
			counts[key]++
			ids[key] = row.Id
		}
	}
	sqlIDs, err := daemonSQLTrafficIDs(db, identities)
	if err != nil {
		return 0, err
	}
	for _, key := range identities {
		if counts[key] != 1 || sqlIDs[key] == 0 || sqlIDs[key] != ids[key] {
			invalid++
		}
	}
	return invalid, nil
}
