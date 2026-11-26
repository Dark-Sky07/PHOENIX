package service

import (
	"errors"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

// FilterDaemonAccountingClients applies the same traffic identity policy to
// canonical clients used by periodic daemon reconciliation. A non-nil empty
// result is deliberate: PPP constructors interpret nil as permission to use
// raw credentials from inbound settings. It is not a final-accounting proof.
func (s *InboundService) FilterDaemonAccountingClients(clients []model.Client) ([]model.Client, error) {
	if len(clients) == 0 {
		return make([]model.Client, 0), nil
	}
	allowed, _, err := daemonAccountingEligibility(database.GetDB(), clients)
	if err != nil {
		return nil, err
	}
	out := make([]model.Client, 0, len(clients))
	for i, client := range clients {
		if allowed[i] {
			out = append(out, client)
		}
	}
	return out, nil
}

// daemonAccountingEligibility works with the caller's transaction and keeps
// decisions per entry, so duplicate credentials with different quota fields
// cannot inherit one another's authorization. It is a point-in-time scan.
func daemonAccountingEligibility(db *gorm.DB, clients []model.Client) ([]bool, []int, error) {
	allowed := make([]bool, len(clients))
	trafficIDs := make([]int, len(clients))
	if db == nil {
		return nil, nil, errors.New("database unavailable while filtering daemon clients")
	}
	keys := make([]string, 0, len(clients))
	for _, client := range clients {
		if client.Enable {
			keys = append(keys, transferEmailKey(client.Email))
		}
	}
	var rows []xray.ClientTraffic
	if err := db.Select("id", "email", "enable", "up", "down", "total", "expiry_time").Find(&rows).Error; err != nil {
		return nil, nil, err
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
		return nil, nil, err
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
		return nil, nil, err
	}
	// Raw inbound settings can lag canonical client records. When canonical
	// evidence exists, it cannot be bypassed by a stale enabled credential.
	// Missing canonical records remain supported for legacy settings-only
	// inbounds; ambiguous normalized records are never safe to choose between.
	var records []model.ClientRecord
	if err := db.Select("id", "email", "enable", "total_gb", "expiry_time", "activate_before").Find(&records).Error; err != nil {
		return nil, nil, err
	}
	recordCounts := make(map[string]int, len(records))
	canonical := make(map[string]model.ClientRecord, len(records))
	for _, record := range records {
		key := transferEmailKey(record.Email)
		recordCounts[key]++
		canonical[key] = record
	}
	now := time.Now().UnixMilli()
	for i, client := range clients {
		key := transferEmailKey(client.Email)
		row := counters[key]
		if !client.Enable || key == "" || counts[key] != 1 || row.Id == 0 || sqlIDs[key] != row.Id || !row.Enable {
			continue
		}
		global := effective[key]
		up, down := max(row.Up, global.up), max(row.Down, global.down)
		record, hasRecord := canonical[key]
		if recordCounts[key] > 1 || (hasRecord && (!record.Enable ||
			localTrafficDepleted(record.TotalGB, up, down) ||
			(record.ExpiryTime > 0 && record.ExpiryTime <= now) ||
			(record.ExpiryTime < 0 && record.ActivateBefore > 0 && record.ActivateBefore <= now))) {
			continue
		}
		if row.Up < 0 || row.Down < 0 || global.invalid ||
			localTrafficDepleted(row.Total, up, down) || localTrafficDepleted(client.TotalGB, up, down) ||
			(row.ExpiryTime > 0 && row.ExpiryTime <= now) || (client.ExpiryTime > 0 && client.ExpiryTime <= now) ||
			(client.ExpiryTime < 0 && client.ActivateBefore > 0 && client.ActivateBefore <= now) {
			continue
		}
		allowed[i] = true
		trafficIDs[i] = row.Id
	}
	return allowed, trafficIDs, nil
}
