package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// PrepareVerifiedInboundSnapshot captures a complete, local-only view for a
// separately provisioned API-token stream, then durably reserves its sequence
// under the allocation lock. It does not poll the runtime or prove that bytes
// between polls were observed. Nested nodes and multi-admin inbounds are
// rejected until their complete accounting semantics can be established.
func (s *InboundService) PrepareVerifiedInboundSnapshot(tokenID, userID int, token string, challenge []byte) (*trafficprotocol.Envelope, error) {
	if tokenID <= 0 || userID <= 0 || len(challenge) != 32 {
		return nil, errors.New("invalid snapshot principal")
	}
	key, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(token)
	if err != nil {
		return nil, err
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		return nil, err
	}
	defer unlock()
	db := database.GetDB()
	var state model.TrafficSnapshotSender
	id := fmt.Sprintf("node-snapshot:api-token:%d", tokenID)
	if err := db.First(&state, "id = ?", id).Error; err != nil {
		return nil, err
	}
	var setting model.Setting
	if err := db.Where("key = ?", "panelGuid").First(&setting).Error; err != nil {
		return nil, err
	}
	if !validTrafficPeerIdentity(state.Sender) || !validTrafficPeerIdentity(state.Generation) || setting.Value != state.Sender {
		return nil, errors.New("node snapshot sender identity is not durably configured")
	}
	// Do not silently omit another administrator's or a descendant node's
	// traffic. Legacy APIs scope to the first administrator; this protocol
	// refuses incomplete coverage instead of attesting a partial snapshot.
	var unsupported int64
	if err := db.Model(&model.Inbound{}).Where("node_id IS NOT NULL OR user_id <> ?", userID).Count(&unsupported).Error; err != nil {
		return nil, err
	}
	if unsupported > 0 {
		return nil, errors.New("node snapshot requires reconciliation of nested or other-admin inbounds")
	}
	inbounds := make([]*model.Inbound, 0)
	if err := db.Model(&model.Inbound{}).Preload("ClientStats").Where("user_id = ?", userID).Order("id ASC").Find(&inbounds).Error; err != nil {
		return nil, err
	}
	var all []xray.ClientTraffic
	if err := db.Model(&xray.ClientTraffic{}).Find(&all).Error; err != nil {
		return nil, err
	}
	byEmail := make(map[string]xray.ClientTraffic, len(all))
	for _, row := range all {
		key := nodeEmailKey(row.Email)
		if key == "" || row.Up < 0 || row.Down < 0 {
			return nil, errors.New("invalid local traffic requires reconciliation")
		}
		if _, exists := byEmail[key]; exists {
			return nil, errors.New("ambiguous local traffic requires reconciliation")
		}
		byEmail[key] = row
	}
	for _, ib := range inbounds {
		if ib == nil || ib.Tag == "" || ib.Tag != strings.TrimSpace(ib.Tag) || ib.Up < 0 || ib.Down < 0 {
			return nil, errors.New("invalid local inbound snapshot")
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ib.Settings), &settings); err != nil || settings == nil {
			return nil, fmt.Errorf("invalid settings for inbound %q", ib.Tag)
		}
		var clients []model.Client
		if raw, ok := settings["clients"]; ok {
			if string(raw) == "null" || json.Unmarshal(raw, &clients) != nil {
				return nil, fmt.Errorf("invalid clients for inbound %q", ib.Tag)
			}
		}
		seen := make(map[string]bool)
		for _, row := range ib.ClientStats {
			key := nodeEmailKey(row.Email)
			if key == "" || seen[key] {
				return nil, errors.New("ambiguous inbound client stats")
			}
			seen[key] = true
		}
		configured := make(map[string]bool, len(clients))
		for _, client := range clients {
			key := nodeEmailKey(client.Email)
			if key == "" {
				return nil, errors.New("client without accounting identity")
			}
			if configured[key] {
				return nil, errors.New("duplicate inbound client identity")
			}
			configured[key] = true
			row, ok := byEmail[key]
			if !ok {
				return nil, fmt.Errorf("missing local traffic for %q", key)
			}
			if !seen[key] {
				ib.ClientStats = append(ib.ClientStats, row)
				seen[key] = true
			}
		}
		ib.OriginNodeGuid = state.Sender
	}
	payload, err := json.Marshal(runtime.TrafficSnapshot{Inbounds: inbounds, Challenge: append([]byte(nil), challenge...)})
	if err != nil {
		return nil, err
	}
	if len(payload) > trafficprotocol.MaxSnapshotBytes {
		return nil, errors.New("node snapshot exceeds size limit")
	}
	envelope, err := trafficprotocol.ReserveSequence(db, id)
	if err != nil {
		return nil, err
	}
	if envelope.Sender != state.Sender || envelope.Generation != state.Generation {
		return nil, errors.New("node snapshot sender changed during capture")
	}
	envelope.Snapshot = payload
	envelope, err = trafficprotocol.Sign(envelope, key)
	if err != nil {
		return nil, err
	}
	return &envelope, nil
}
