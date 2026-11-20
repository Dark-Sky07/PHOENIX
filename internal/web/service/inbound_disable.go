package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/xray"

	"gorm.io/gorm"
)

func (s *InboundService) disableInvalidInbounds(tx *gorm.DB) (bool, int64, error) {
	now := time.Now().Unix() * 1000
	needRestart := false

	if p != nil {
		var tags []string
		err := tx.Table("inbounds").
			Select("inbounds.tag").
			Where("((total > 0 and up + down >= total) or (expiry_time > 0 and expiry_time <= ?)) and enable = ? and node_id IS NULL AND protocol NOT IN (?, ?, ?, ?)", now, true, model.OpenVPN, model.L2TP, model.SSTP, model.PPTP).
			Scan(&tags).Error
		if err != nil {
			return false, 0, err
		}
		if len(tags) > 0 {
			if err := s.xrayApi.Init(p.GetAPIPort()); err != nil {
				logger.Warning("Cannot initialize Xray API to disable inbounds:", err)
				needRestart = true
			} else {
				for _, tag := range tags {
					if err := s.xrayApi.DelInbound(tag); err != nil {
						logger.Debug("Error in disabling inbound by api:", err)
						needRestart = true
					} else {
						logger.Debug("Inbound disabled by api:", tag)
					}
				}
				s.xrayApi.Close()
			}
		}
	}

	result := tx.Model(model.Inbound{}).
		Where("((total > 0 and up + down >= total) or (expiry_time > 0 and expiry_time <= ?)) and enable = ? and node_id IS NULL", now, true).
		Update("enable", false)
	err := result.Error
	count := result.RowsAffected
	return needRestart, count, err
}

// The effective usage is the per-direction maximum of local counters and all
// master snapshots, matching overlayGlobalTraffic and reseller accounting.
// Use subtraction rather than adding counters: PostgreSQL bigint addition can
// overflow and SQLite may round oversized integers to float.
const maxGlobalUp = `COALESCE((SELECT MAX(g.up) FROM client_global_traffics g WHERE LOWER(TRIM(g.email)) = LOWER(TRIM(client_traffics.email))), 0)`
const maxGlobalDown = `COALESCE((SELECT MAX(g.down) FROM client_global_traffics g WHERE LOWER(TRIM(g.email)) = LOWER(TRIM(client_traffics.email))), 0)`
const effectiveUp = `(CASE WHEN client_traffics.up > ` + maxGlobalUp + ` THEN client_traffics.up ELSE ` + maxGlobalUp + ` END)`
const effectiveDown = `(CASE WHEN client_traffics.down > ` + maxGlobalDown + ` THEN client_traffics.down ELSE ` + maxGlobalDown + ` END)`
const depletedClientsCond = `((total > 0 AND (` + effectiveUp + ` >= total OR ` + effectiveDown + ` >= total - ` + effectiveUp + `)) OR (expiry_time > 0 AND expiry_time <= ?))`

func (s *InboundService) disableInvalidClients(tx *gorm.DB) (bool, int64, []int, error) {
	now := time.Now().Unix() * 1000
	needRestart := false

	var depletedRows []xray.ClientTraffic
	err := tx.Model(xray.ClientTraffic{}).
		Where(depletedClientsCond+" AND enable = ?", now, true).
		Find(&depletedRows).Error
	if err != nil {
		return false, 0, nil, err
	}
	if len(depletedRows) == 0 {
		return false, 0, nil, nil
	}

	depletedEmails := make([]string, 0, len(depletedRows))
	for i := range depletedRows {
		if depletedRows[i].Email == "" {
			continue
		}
		depletedEmails = append(depletedEmails, transferEmailKey(depletedRows[i].Email))
	}

	type target struct {
		InboundID int  `gorm:"column:inbound_id"`
		NodeID    *int `gorm:"column:node_id"`
		Tag       string
		Email     string
		Protocol  model.Protocol `gorm:"column:protocol"`
	}
	var targets []target
	if len(depletedEmails) > 0 {
		err = tx.Raw(`
			SELECT inbounds.id AS inbound_id, inbounds.node_id AS node_id,
			       inbounds.tag AS tag, inbounds.protocol AS protocol,
			       clients.email AS email
			FROM clients
			JOIN client_inbounds ON client_inbounds.client_id = clients.id
			JOIN inbounds        ON inbounds.id = client_inbounds.inbound_id
			WHERE LOWER(TRIM(clients.email)) IN ?
		`, depletedEmails).Scan(&targets).Error
		if err != nil {
			return false, 0, nil, err
		}
	}

	var localTargets []target
	localByInbound := make(map[int]map[string]struct{})
	remoteByInbound := make(map[int][]target)
	for _, t := range targets {
		if t.NodeID == nil {
			localTargets = append(localTargets, t)
			if localByInbound[t.InboundID] == nil {
				localByInbound[t.InboundID] = make(map[string]struct{})
			}
			localByInbound[t.InboundID][transferEmailKey(t.Email)] = struct{}{}
		} else {
			t.Email = strings.TrimSpace(t.Email)
			remoteByInbound[t.InboundID] = append(remoteByInbound[t.InboundID], t)
		}
	}

	if p != nil && len(localTargets) > 0 {
		if err := s.xrayApi.Init(p.GetAPIPort()); err != nil {
			logger.Warning("Cannot initialize Xray API to disable clients:", err)
			needRestart = true
		} else {
			for _, t := range localTargets {
				// OpenVPN and L2TP are reconciled by their host-daemon jobs, not
				// by the Xray API. The canonical client/stat rows and settings
				// below are the source of truth for the next daemon reconcile.
				if t.Protocol == model.OpenVPN || t.Protocol == model.L2TP || t.Protocol == model.SSTP || t.Protocol == model.PPTP {
					continue
				}
				err1 := s.xrayApi.RemoveUser(t.Tag, t.Email)
				if err1 == nil {
					logger.Debug("Client disabled by api:", t.Email)
				} else if strings.Contains(err1.Error(), fmt.Sprintf("User %s not found.", t.Email)) {
					logger.Debug("User is already disabled. Nothing to do more...")
				} else {
					logger.Debug("Error in disabling client by api:", err1)
					needRestart = true
				}
			}
			s.xrayApi.Close()
		}
	}

	for inboundID, emails := range localByInbound {
		if _, _, mErr := s.markClientsDisabledInSettings(tx, inboundID, emails); mErr != nil {
			return true, 0, nil, fmt.Errorf("disable client settings for inbound %d: %w", inboundID, mErr)
		}
	}

	result := tx.Model(xray.ClientTraffic{}).
		Where(depletedClientsCond+" AND enable = ?", now, true).
		Update("enable", false)
	err = result.Error
	count := result.RowsAffected
	if err != nil {
		return needRestart, count, nil, err
	}

	if len(depletedEmails) > 0 {
		if err := tx.Model(&model.ClientRecord{}).
			Where("LOWER(TRIM(email)) IN ?", depletedEmails).
			Updates(map[string]any{"enable": false, "updated_at": now}).Error; err != nil {
			return true, 0, nil, fmt.Errorf("disable client records: %w", err)
		}
	}

	disabledNodeIDs := make(map[int]struct{})
	for inboundID, group := range remoteByInbound {
		emails := make(map[string]struct{}, len(group))
		for _, t := range group {
			emails[transferEmailKey(t.Email)] = struct{}{}
		}
		if pushErr := s.disableRemoteClients(tx, inboundID, emails); pushErr != nil {
			logger.Warning("disableInvalidClients: push to remote failed for inbound", inboundID, ":", pushErr)
			needRestart = true
		} else {
			for _, t := range group {
				if t.NodeID != nil {
					disabledNodeIDs[*t.NodeID] = struct{}{}
				}
			}
		}
	}

	nodeIDs := make([]int, 0, len(disabledNodeIDs))
	for nodeID := range disabledNodeIDs {
		nodeIDs = append(nodeIDs, nodeID)
	}

	return needRestart, count, nodeIDs, nil
}

// markClientsDisabledInSettings flips client.enable=false in the inbound's
// stored settings JSON for the given emails and returns both the pre and
// post snapshots so a caller pushing to a remote node has the diff to hand.
func (s *InboundService) markClientsDisabledInSettings(tx *gorm.DB, inboundID int, emails map[string]struct{}) (oldIb, newIb *model.Inbound, err error) {
	var ib model.Inbound
	if err := tx.Model(&model.Inbound{}).Where("id = ?", inboundID).First(&ib).Error; err != nil {
		return nil, nil, err
	}
	snapshot := ib

	settings := map[string]any{}
	if !json.Valid([]byte(ib.Settings)) {
		return nil, nil, fmt.Errorf("invalid inbound settings JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(ib.Settings))
	decoder.UseNumber()
	if err := decoder.Decode(&settings); err != nil {
		return nil, nil, err
	}
	clients, ok := settings["clients"].([]any)
	if !ok {
		return nil, nil, fmt.Errorf("inbound %d has invalid clients", inboundID)
	}
	now := time.Now().Unix() * 1000
	mutated := false
	matched := make(map[string]bool, len(emails))
	for i := range clients {
		entry, ok := clients[i].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("inbound %d has invalid client", inboundID)
		}
		email, ok := entry["email"].(string)
		if !ok || transferEmailKey(email) == "" {
			return nil, nil, fmt.Errorf("inbound %d has client without email", inboundID)
		}
		key := transferEmailKey(email)
		if _, hit := emails[key]; !hit {
			continue
		}
		matched[key] = true
		if cur, exists := entry["enable"].(bool); exists && !cur {
			continue
		}
		entry["enable"] = false
		entry["updated_at"] = now
		clients[i] = entry
		mutated = true
	}
	for email := range emails {
		if !matched[email] {
			return nil, nil, fmt.Errorf("client %q missing from inbound %d settings", email, inboundID)
		}
	}
	if !mutated {
		return &snapshot, &ib, nil
	}
	settings["clients"] = clients
	bs, marshalErr := json.MarshalIndent(settings, "", "  ")
	if marshalErr != nil {
		return nil, nil, marshalErr
	}
	ib.Settings = string(bs)
	if err := tx.Model(&model.Inbound{}).Where("id = ?", inboundID).
		Update("settings", ib.Settings).Error; err != nil {
		return nil, nil, err
	}
	return &snapshot, &ib, nil
}

func (s *InboundService) disableRemoteClients(tx *gorm.DB, inboundID int, emails map[string]struct{}) error {
	oldSnapshot, ib, err := s.markClientsDisabledInSettings(tx, inboundID, emails)
	if err != nil {
		return err
	}

	rt, err := s.runtimeFor(ib)
	if err != nil {
		return err
	}
	if err := rt.UpdateInbound(context.Background(), oldSnapshot, ib); err != nil {
		return err
	}
	return nil
}
