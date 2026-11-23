package service

import (
	"errors"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// FilterDaemonAccountingClients applies the same traffic identity policy to
// canonical clients used by periodic daemon reconciliation. A non-nil empty
// result is deliberate: PPP constructors interpret nil as permission to use
// raw credentials from inbound settings. It is not a final-accounting proof.
func (s *InboundService) FilterDaemonAccountingClients(clients []model.Client) ([]model.Client, error) {
	out := make([]model.Client, 0, len(clients))
	if len(clients) == 0 {
		return out, nil
	}
	db := database.GetDB()
	if db == nil {
		return nil, errors.New("database unavailable while filtering daemon clients")
	}
	keys := make([]string, 0, len(clients))
	for _, client := range clients {
		if client.Enable {
			keys = append(keys, transferEmailKey(client.Email))
		}
	}
	var rows []xray.ClientTraffic
	if err := db.Select("id", "email", "enable").Find(&rows).Error; err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(rows))
	ids := make(map[string]int, len(rows))
	enabled := make(map[string]bool, len(rows))
	for _, row := range rows {
		key := transferEmailKey(row.Email)
		counts[key]++
		ids[key] = row.Id
		enabled[key] = row.Enable
	}
	sqlIDs, err := daemonSQLTrafficIDs(db, keys)
	if err != nil {
		return nil, err
	}
	for _, client := range clients {
		key := transferEmailKey(client.Email)
		if client.Enable && key != "" && counts[key] == 1 && ids[key] != 0 && sqlIDs[key] == ids[key] && enabled[key] {
			out = append(out, client)
		}
	}
	return out, nil
}
