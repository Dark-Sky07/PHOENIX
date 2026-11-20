package service

import (
	"fmt"

	"gorm.io/gorm"
)

// validateResellerNodeBaselines prevents allocation or renewal while an
// attached reseller client's node counters have no trustworthy starting point.
// It does not establish that the node is online or that its latest usage has
// already been received. The caller must serialize allocation mutations.
func validateResellerNodeBaselines(tx *gorm.DB, resellerID int) error {
	var incidents int64
	err := tx.Table("reseller_node_counter_incidents AS incident").
		Joins("JOIN reseller_clients rc ON LOWER(TRIM(rc.email)) = LOWER(TRIM(incident.email))").
		Where("rc.reseller_id = ?", resellerID).Count(&incidents).Error
	if err != nil {
		return err
	}
	if incidents > 0 {
		return fmt.Errorf("reseller node counter incident requires manual reconciliation")
	}
	type row struct {
		NodeID     int    `gorm:"column:node_id"`
		Email      string `gorm:"column:email"`
		BaselineID *int   `gorm:"column:baseline_id"`
		Up         *int64 `gorm:"column:baseline_up"`
		Down       *int64 `gorm:"column:baseline_down"`
	}
	var rows []row
	err = tx.Table("reseller_clients AS rc").
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
