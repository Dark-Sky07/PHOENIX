package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database/model"

	"gorm.io/gorm"
)

// validateResellerNodeBaselines prevents allocation or renewal while an
// attached reseller client's node counters have no trustworthy starting point.
// It does not establish that the node is online or that its latest usage has
// already been received. The caller must serialize allocation mutations.
func validateResellerNodeBaselines(tx *gorm.DB, resellerID int) error {
	if err := rejectResellerNodeIncidents(tx, resellerID, nil); err != nil {
		return err
	}
	type row struct {
		NodeID     int    `gorm:"column:node_id"`
		Email      string `gorm:"column:email"`
		BaselineID *int   `gorm:"column:baseline_id"`
		Up         *int64 `gorm:"column:baseline_up"`
		Down       *int64 `gorm:"column:baseline_down"`
	}
	var rows []row
	err := tx.Table("reseller_clients AS rc").
		Select("i.node_id AS node_id, c.email AS email, b.id AS baseline_id, b.up AS baseline_up, b.down AS baseline_down").
		Joins("JOIN clients c ON LOWER(TRIM(c.email)) = LOWER(TRIM(rc.email))").
		Joins("JOIN client_inbounds ci ON ci.client_id = c.id").
		Joins("JOIN inbounds i ON i.id = ci.inbound_id AND i.node_id IS NOT NULL").
		Joins("LEFT JOIN node_client_traffics b ON b.node_id = i.node_id AND LOWER(TRIM(b.email)) = LOWER(TRIM(c.email))").
		Where("rc.reseller_id = ?", resellerID).Scan(&rows).Error
	if err != nil {
		return err
	}
	type identity struct {
		node  int
		email string
	}
	seen := make(map[identity]int)
	for _, r := range rows {
		key := identity{node: r.NodeID, email: nodeEmailKey(r.Email)}
		if r.BaselineID == nil || r.Up == nil || r.Down == nil || *r.Up < 0 || *r.Down < 0 {
			return fmt.Errorf("reseller node traffic baseline missing or invalid for %q on node %d; manual reconciliation required", r.Email, r.NodeID)
		}
		if previous, ok := seen[key]; ok && previous != *r.BaselineID {
			return fmt.Errorf("reseller node traffic baseline ambiguous for %q on node %d; manual reconciliation required", r.Email, r.NodeID)
		}
		seen[key] = *r.BaselineID
	}
	return nil
}

// A nil email slice checks all mappings. A non-nil batch checks only those
// mappings so an unrelated reseller's incident does not block a no-op.
func rejectResellerNodeIncidents(tx *gorm.DB, resellerID int, emails []string) error {
	if emails != nil && len(emails) == 0 {
		return nil
	}
	q := tx.Table("reseller_node_counter_incidents AS incident").
		Joins("JOIN reseller_clients rc ON LOWER(TRIM(rc.email)) = LOWER(TRIM(incident.email))").
		Where("rc.reseller_id = ?", resellerID)
	if emails != nil {
		q = q.Where("LOWER(TRIM(rc.email)) IN ?", emails)
	}
	var count int64
	if err := q.Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("reseller node counter incident requires manual reconciliation")
	}
	return nil
}

// rejectClientNodeIncidents is used before destructive runtime calls and again
// in the accounting transaction. A marker stays significant even if ownership
// has already drifted or the canonical client record is missing.
func rejectClientNodeIncidents(tx *gorm.DB, emails []string) error {
	keys := make([]string, 0, len(emails))
	for _, email := range emails {
		if key := transferEmailKey(email); key != "" {
			keys = append(keys, key)
		}
	}
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var count int64
		if err := tx.Table("reseller_node_counter_incidents").Where("LOWER(TRIM(email)) IN ?", batch).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("node counter incident requires manual reconciliation before client removal")
		}
	}
	return nil
}

// Inbound removal affects runtime and links before its DB cleanup. Check all
// three identity sources first, including older settings-only associations.
func rejectInboundNodeIncidents(tx *gorm.DB, inbound *model.Inbound) error {
	var incidents int64
	if err := tx.Table("reseller_node_counter_incidents").Count(&incidents).Error; err != nil {
		return err
	}
	if incidents == 0 {
		return nil
	}
	if inbound.NodeID != nil {
		var nodeIncidents int64
		if err := tx.Table("reseller_node_counter_incidents").Where("node_id = ?", *inbound.NodeID).Count(&nodeIncidents).Error; err != nil {
			return err
		}
		if nodeIncidents > 0 {
			return fmt.Errorf("node counter incident requires manual reconciliation before inbound removal")
		}
	}
	var emails []string
	if err := tx.Table("clients c").Joins("JOIN client_inbounds ci ON ci.client_id = c.id").Where("ci.inbound_id = ?", inbound.Id).Pluck("c.email", &emails).Error; err != nil {
		return err
	}
	var trafficEmails []string
	if err := tx.Table("client_traffics").Where("inbound_id = ?", inbound.Id).Pluck("email", &trafficEmails).Error; err != nil {
		return err
	}
	emails = append(emails, trafficEmails...)
	if strings.TrimSpace(inbound.Settings) != "" {
		var settings struct {
			Clients []struct {
				Email string `json:"email"`
			} `json:"clients"`
		}
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
			return fmt.Errorf("invalid inbound settings; cannot rule out a node counter incident: %w", err)
		}
		for _, client := range settings.Clients {
			emails = append(emails, client.Email)
		}
	}
	return rejectClientNodeIncidents(tx, emails)
}
