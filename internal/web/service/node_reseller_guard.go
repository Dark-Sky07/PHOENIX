package service

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

// A node snapshot is not proof that final traffic was observed. Do not let a
// snapshot detach a reseller client or replace its quota while its allocation
// could still cover unpolled traffic. It also blocks newly reported reseller
// attachments; it does not fence changes made directly on the node.
func validateNodeResellerSnapshot(tx *gorm.DB, nodeID int, snap *runtime.TrafficSnapshot, tagToCentral map[string]*model.Inbound, dirty bool) error {
	type attachment struct {
		InboundID int
		Email     string
		TotalGB   int64
	}
	var attached []attachment
	if err := tx.Table("client_inbounds").
		Select("client_inbounds.inbound_id, clients.email, clients.total_gb").
		Joins("JOIN clients ON clients.id = client_inbounds.client_id").
		Joins("JOIN inbounds ON inbounds.id = client_inbounds.inbound_id").
		Where("inbounds.node_id = ?", nodeID).
		Where("EXISTS (SELECT 1 FROM reseller_clients rc WHERE LOWER(TRIM(rc.email)) = LOWER(TRIM(clients.email)))").
		Scan(&attached).Error; err != nil {
		return err
	}
	attachedKeys := make(map[int]map[string]bool)
	for _, link := range attached {
		if attachedKeys[link.InboundID] == nil {
			attachedKeys[link.InboundID] = make(map[string]bool)
		}
		attachedKeys[link.InboundID][nodeEmailKey(link.Email)] = true
	}
	if err := rejectNewNodeResellerAssociations(tx, snap, tagToCentral, attachedKeys); err != nil {
		return err
	}
	if len(attached) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attached))
	seenKeys := make(map[string]bool, len(attached))
	for _, link := range attached {
		key := nodeEmailKey(link.Email)
		if !seenKeys[key] {
			seenKeys[key] = true
			keys = append(keys, key)
		}
	}
	localStats := make(map[string]xray.ClientTraffic, len(keys))
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var rows []xray.ClientTraffic
		if err := tx.Where("LOWER(TRIM(email)) IN ?", batch).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			key := nodeEmailKey(row.Email)
			if _, exists := localStats[key]; exists {
				return fmt.Errorf("ambiguous reseller traffic for %q", key)
			}
			localStats[key] = row
		}
	}
	byInbound := make(map[int]*model.Inbound)
	stats := make(map[string][]xray.ClientTraffic)
	for _, remote := range snap.Inbounds {
		if remote == nil {
			continue
		}
		if central := tagToCentral[remote.Tag]; central != nil {
			if byInbound[central.Id] != nil {
				return fmt.Errorf("duplicate node inbound snapshot %d", central.Id)
			}
			byInbound[central.Id] = remote
		}
		for _, traffic := range remote.ClientStats {
			key := nodeEmailKey(traffic.Email)
			stats[key] = append(stats[key], traffic)
		}
	}
	clientsByInbound := make(map[int]map[string]model.Client)
	originalByInbound := make(map[int]map[string]model.Client)
	for _, link := range attached {
		key := nodeEmailKey(link.Email)
		remote := byInbound[link.InboundID]
		if remote == nil {
			if dirty {
				continue
			}
			return fmt.Errorf("reseller client %q missing node inbound; manual reconciliation required", link.Email)
		}
		if !dirty {
			clients, ok := clientsByInbound[link.InboundID]
			if !ok {
				var settings struct {
					Clients []model.Client `json:"clients"`
				}
				if err := json.Unmarshal([]byte(remote.Settings), &settings); err != nil || settings.Clients == nil {
					return fmt.Errorf("invalid node client settings for inbound %d; manual reconciliation required", link.InboundID)
				}
				clients = make(map[string]model.Client, len(settings.Clients))
				for _, client := range settings.Clients {
					k := nodeEmailKey(client.Email)
					if k == "" {
						return fmt.Errorf("node client without email in inbound %d", link.InboundID)
					}
					if _, duplicate := clients[k]; duplicate {
						return fmt.Errorf("duplicate node client in inbound %d", link.InboundID)
					}
					clients[k] = client
				}
				clientsByInbound[link.InboundID] = clients
			}
			client, ok := clients[key]
			if !ok || client.TotalGB != link.TotalGB {
				return fmt.Errorf("reseller client %q missing or changed node quota; manual reconciliation required", link.Email)
			}
			original, ok := originalByInbound[link.InboundID]
			if !ok {
				var stored model.Inbound
				if err := tx.Select("settings").First(&stored, link.InboundID).Error; err != nil {
					return err
				}
				var settings struct {
					Clients []model.Client `json:"clients"`
				}
				if err := json.Unmarshal([]byte(stored.Settings), &settings); err != nil || settings.Clients == nil {
					return fmt.Errorf("invalid stored reseller inbound %d settings", link.InboundID)
				}
				original = make(map[string]model.Client, len(settings.Clients))
				for _, entry := range settings.Clients {
					key := nodeEmailKey(entry.Email)
					if _, dup := original[key]; dup {
						return fmt.Errorf("ambiguous stored reseller inbound %d settings", link.InboundID)
					}
					original[key] = entry
				}
				originalByInbound[link.InboundID] = original
			}
			prior, ok := original[key]
			if !ok || !sameNodeResellerClientSecurity(prior, client) {
				return fmt.Errorf("reseller client %q node settings changed; manual reconciliation required", link.Email)
			}
			if len(stats[key]) == 0 {
				return fmt.Errorf("reseller client %q missing node traffic; manual reconciliation required", link.Email)
			}
		}
		local, exists := localStats[key]
		if !exists {
			return fmt.Errorf("reseller client %q has no local traffic counter; manual reconciliation required", link.Email)
		}
		for _, traffic := range stats[key] {
			if traffic.Total != link.TotalGB || traffic.ExpiryTime != local.ExpiryTime || traffic.Reset != local.Reset {
				return fmt.Errorf("reseller client %q node traffic limits changed; manual reconciliation required", link.Email)
			}
		}
	}
	return nil
}

// A newly introduced node association has not been authorized or allocated
// by the reseller creation workflow. Block it before SyncInbound can overwrite
// an existing reseller client's canonical quota or other fields.
func rejectNewNodeResellerAssociations(tx *gorm.DB, snap *runtime.TrafficSnapshot, tagToCentral map[string]*model.Inbound, attached map[int]map[string]bool) error {
	type occurrence struct {
		inboundID int
		key       string
	}
	var occurrences []occurrence
	keys := make(map[string]struct{})
	for _, inbound := range snap.Inbounds {
		if inbound == nil {
			continue
		}
		id := 0
		if central := tagToCentral[inbound.Tag]; central != nil {
			id = central.Id
		}
		add := func(email string) {
			key := nodeEmailKey(email)
			if key == "" {
				return
			}
			keys[key] = struct{}{}
			occurrences = append(occurrences, occurrence{inboundID: id, key: key})
		}
		for _, traffic := range inbound.ClientStats {
			add(traffic.Email)
		}
		if inbound.Settings == "" {
			continue
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
			return fmt.Errorf("invalid node inbound settings for %q: %w", inbound.Tag, err)
		}
		if raw, ok := settings["clients"]; ok && string(raw) != "null" {
			var clients []struct {
				Email string `json:"email"`
			}
			if err := json.Unmarshal(raw, &clients); err != nil {
				return fmt.Errorf("invalid node clients for %q: %w", inbound.Tag, err)
			}
			for _, client := range clients {
				add(client.Email)
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	emails := make([]string, 0, len(keys))
	for key := range keys {
		emails = append(emails, key)
	}
	mapped := make(map[string]bool)
	for _, batch := range chunkStrings(emails, sqlInChunk) {
		var mappings []model.ResellerClient
		if err := tx.Where("LOWER(TRIM(email)) IN ?", batch).Find(&mappings).Error; err != nil {
			return err
		}
		for _, mapping := range mappings {
			mapped[nodeEmailKey(mapping.Email)] = true
		}
	}
	for _, occurrence := range occurrences {
		if mapped[occurrence.key] && !attached[occurrence.inboundID][occurrence.key] {
			return fmt.Errorf("new node association for reseller client %q requires manual reconciliation", occurrence.key)
		}
	}
	return nil
}

// Node display metadata may change independently, but authorization, quota,
// expiry and renewal settings must match the last accepted inbound snapshot.
func sameNodeResellerClientSecurity(a, b model.Client) bool {
	a.Email, b.Email = nodeEmailKey(a.Email), nodeEmailKey(b.Email)
	a.Comment, b.Comment = "", ""
	a.Group, b.Group = "", ""
	a.CreatedAt, b.CreatedAt = 0, 0
	a.UpdatedAt, b.UpdatedAt = 0, 0
	a.SubLastUpdatedAt, b.SubLastUpdatedAt = 0, 0
	a.SubLastUserAgent, b.SubLastUserAgent = "", ""
	return reflect.DeepEqual(a, b)
}
