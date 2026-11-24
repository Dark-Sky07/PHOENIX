package service

import (
	"errors"
	"time"

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
	if err := db.Select("id", "email", "enable", "up", "down", "total", "expiry_time").Find(&rows).Error; err != nil {
		return nil, err
	}
	counts := make(map[string]int, len(rows))
	counters := make(map[string]xray.ClientTraffic, len(rows))
	for _, row := range rows {
		key := transferEmailKey(row.Email)
		counts[key]++
		counters[key] = row
	}
	// SQL LOWER/TRIM does not implement Go's Unicode/whitespace identity
	// normalization. Inspect all global snapshots in Go so a hidden variant
	// cannot make a consumed or corrupt quota appear unused.
	var globals []model.ClientGlobalTraffic
	if err := db.Select("email", "up", "down").Find(&globals).Error; err != nil {
		return nil, err
	}
	type usage struct {
		up, down int64
		invalid  bool
	}
	effective := make(map[string]usage, len(globals))
	for _, row := range globals {
		key := transferEmailKey(row.Email)
		v := effective[key]
		v.invalid = v.invalid || row.Up < 0 || row.Down < 0
		v.up = max(v.up, row.Up)
		v.down = max(v.down, row.Down)
		effective[key] = v
	}
	sqlIDs, err := daemonSQLTrafficIDs(db, keys)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	for _, client := range clients {
		key := transferEmailKey(client.Email)
		row := counters[key]
		if !client.Enable || key == "" || counts[key] != 1 || row.Id == 0 || sqlIDs[key] != row.Id || !row.Enable {
			continue
		}
		global := effective[key]
		up, down := max(row.Up, global.up), max(row.Down, global.down)
		if row.Up < 0 || row.Down < 0 || global.invalid ||
			localTrafficDepleted(row.Total, up, down) || localTrafficDepleted(client.TotalGB, up, down) ||
			(row.ExpiryTime > 0 && row.ExpiryTime <= now) || (client.ExpiryTime > 0 && client.ExpiryTime <= now) {
			continue
		}
		out = append(out, client)
	}
	return out, nil
}
