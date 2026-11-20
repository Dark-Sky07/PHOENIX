package service

import (
	"encoding/json"
	"fmt"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"gorm.io/gorm"
)

// A node snapshot is not proof that final traffic was observed. Do not let a
// snapshot detach a reseller client or replace its quota while its allocation
// could still cover unpolled traffic. This guard covers existing attachments;
// it does not fence changes made directly on the node.
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
	if len(attached) == 0 {
		return nil
	}
	byInbound := make(map[int]*model.Inbound)
	stats := make(map[string][]int64)
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
			stats[key] = append(stats[key], traffic.Total)
		}
	}
	clientsByInbound := make(map[int]map[string]int64)
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
				clients = make(map[string]int64, len(settings.Clients))
				for _, client := range settings.Clients {
					k := nodeEmailKey(client.Email)
					if k == "" {
						return fmt.Errorf("node client without email in inbound %d", link.InboundID)
					}
					if _, duplicate := clients[k]; duplicate {
						return fmt.Errorf("duplicate node client in inbound %d", link.InboundID)
					}
					clients[k] = client.TotalGB
				}
				clientsByInbound[link.InboundID] = clients
			}
			if quota, ok := clients[key]; !ok || quota != link.TotalGB {
				return fmt.Errorf("reseller client %q missing or changed node quota; manual reconciliation required", link.Email)
			}
			if len(stats[key]) == 0 {
				return fmt.Errorf("reseller client %q missing node traffic; manual reconciliation required", link.Email)
			}
		}
		for _, quota := range stats[key] {
			if quota != link.TotalGB {
				return fmt.Errorf("reseller client %q node traffic quota mismatch; manual reconciliation required", link.Email)
			}
		}
	}
	return nil
}
