package service

import (
	"errors"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

type renewalQuotaSnapshot struct {
	limit, allocated, used int64
	currentUsage           map[int]int64
	currentQuota           map[int]int64
	disabled               bool
}

type renewalQuotaTracker struct {
	tx       *gorm.DB
	accounts map[int]*renewalQuotaSnapshot
}

// load uses the same max(current usage, allocation, current-window archive)
// rule as Stat, plus historical windows, in the writer's DB transaction.
// Duplicate or dangling ownership is rejected rather than undercounted.
func (t *renewalQuotaTracker) load(id int) (*renewalQuotaSnapshot, error) {
	if found := t.accounts[id]; found != nil {
		return found, nil
	}
	var reseller model.Reseller
	if err := t.tx.First(&reseller, id).Error; err != nil {
		return nil, err
	}
	state := &renewalQuotaSnapshot{limit: reseller.TrafficLimit, currentUsage: map[int]int64{}, currentQuota: map[int]int64{}, disabled: !reseller.Enable || IsExpired(reseller.ExpiryTime)}
	var mappings []model.ResellerClient
	if err := t.tx.Where("reseller_id = ?", id).Find(&mappings).Error; err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(mappings))
	seen := map[string]struct{}{}
	for _, m := range mappings {
		key := transferEmailKey(m.Email)
		if key == "" {
			return nil, errors.New("empty reseller client mapping")
		}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	clients := map[int]model.ClientRecord{}
	clientByEmail := map[string]int{}
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var rows []model.ClientRecord
		if err := t.tx.Where("LOWER(TRIM(email)) IN ?", batch).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, c := range rows {
			key := transferEmailKey(c.Email)
			if _, exists := clientByEmail[key]; exists {
				return nil, errors.New("ambiguous reseller client")
			}
			clientByEmail[key] = c.Id
			clients[c.Id] = c
		}
		var traffic []xray.ClientTraffic
		if err := t.tx.Where("LOWER(TRIM(email)) IN ?", batch).Find(&traffic).Error; err != nil {
			return nil, err
		}
		var globals []model.ClientGlobalTraffic
		if err := t.tx.Where("LOWER(TRIM(email)) IN ?", batch).Find(&globals).Error; err != nil {
			return nil, err
		}
		up, down := map[string]int64{}, map[string]int64{}
		for _, g := range globals {
			key := transferEmailKey(g.Email)
			up[key] = max(up[key], g.Up)
			down[key] = max(down[key], g.Down)
		}
		seenTraffic := map[string]bool{}
		for _, row := range traffic {
			key := transferEmailKey(row.Email)
			seenTraffic[key] = true
			if cid, ok := clientByEmail[key]; ok {
				state.currentUsage[cid] = saturatingAddBytes(state.currentUsage[cid], saturatingAddBytes(max(row.Up, up[key]), max(row.Down, down[key])))
			}
		}
		for _, key := range batch {
			if cid, ok := clientByEmail[key]; ok && !seenTraffic[key] {
				state.currentUsage[cid] = saturatingAddBytes(up[key], down[key])
			}
		}
	}
	for _, key := range keys {
		if _, ok := clientByEmail[key]; !ok {
			return nil, errors.New("reseller client mapping has no client record")
		}
	}
	var archives []model.ResellerTrafficArchive
	if err := t.tx.Where("reseller_id = ?", id).Find(&archives).Error; err != nil {
		return nil, err
	}
	for _, a := range archives {
		state.allocated = saturatingAddBytes(state.allocated, a.PriorConsumedBytes)
		state.used = saturatingAddBytes(state.used, a.PriorConsumedBytes)
		if _, ok := clients[a.ClientRecordId]; ok {
			state.currentUsage[a.ClientRecordId] = max(state.currentUsage[a.ClientRecordId], a.ConsumedBytes)
		} else {
			state.allocated = saturatingAddBytes(state.allocated, a.ConsumedBytes)
			state.used = saturatingAddBytes(state.used, a.ConsumedBytes)
		}
	}
	for id, c := range clients {
		state.used = saturatingAddBytes(state.used, state.currentUsage[id])
		state.currentQuota[id] = max(c.TotalGB, 0)
		state.allocated = saturatingAddBytes(state.allocated, max(state.currentQuota[id], state.currentUsage[id]))
	}
	t.accounts[id] = state
	return state, nil
}

// reserve checks the additional allocation required after the old usage window
// closes. Accepted reservations are retained for the remainder of this tick.
func (t *renewalQuotaTracker) reserve(email string) (bool, error) {
	key := transferEmailKey(email)
	var mappings []model.ResellerClient
	if err := t.tx.Where("LOWER(TRIM(email)) = ?", key).Find(&mappings).Error; err != nil {
		return false, err
	}
	if len(mappings) == 0 {
		return true, nil
	}
	var clients []model.ClientRecord
	if err := t.tx.Where("LOWER(TRIM(email)) = ?", key).Find(&clients).Error; err != nil {
		return false, err
	}
	if len(clients) != 1 {
		return false, errors.New("reseller renewal has ambiguous client")
	}
	reservations := map[int]int64{}
	for _, m := range mappings {
		if _, exists := reservations[m.ResellerId]; exists {
			continue
		}
		state, err := t.load(m.ResellerId)
		if err != nil {
			return false, err
		}
		quota, ok := state.currentQuota[clients[0].Id]
		if !ok {
			return false, errors.New("reseller renewal ownership changed")
		}
		if state.disabled || (state.limit > 0 && quota == 0) {
			return false, nil
		}
		delta := min(quota, state.currentUsage[clients[0].Id])
		if state.limit > 0 && (state.used >= state.limit || state.allocated > state.limit || delta > state.limit-state.allocated) {
			return false, nil
		}
		reservations[m.ResellerId] = delta
	}
	for id, delta := range reservations {
		t.accounts[id].allocated = saturatingAddBytes(t.accounts[id].allocated, delta)
	}
	return true, nil
}
