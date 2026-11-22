package service

import (
	"errors"

	"gorm.io/gorm"
)

// rejectResellerUnverifiedNodes blocks participating quota mutations for any
// reseller assigned a remote inbound or owning a client attached to one.
// Verified outbound global pushes do not authenticate the reverse node traffic
// snapshots, so every remote node remains unverified for this purpose. The
// caller must hold its normal allocation lock when mutation follows this check.
// This does not cover administrator/DB writes, missing associations or remote
// runtimes that are no longer represented in these tables.
func rejectResellerUnverifiedNodes(tx *gorm.DB, resellerID int) error {
	if tx == nil || resellerID <= 0 {
		return errors.New("cannot verify reseller node accounting")
	}
	var assigned int64
	if err := tx.Table("reseller_inbounds AS ri").Joins("JOIN inbounds i ON i.id = ri.inbound_id").Where("ri.reseller_id = ? AND i.node_id IS NOT NULL", resellerID).Count(&assigned).Error; err != nil {
		return err
	}
	if assigned > 0 {
		return errors.New("reseller has a remote inbound whose traffic snapshots require verification and reconciliation")
	}
	var attached int64
	if err := tx.Table("reseller_clients AS rc").Joins("JOIN clients c ON LOWER(TRIM(c.email)) = LOWER(TRIM(rc.email))").Joins("JOIN client_inbounds ci ON ci.client_id = c.id").Joins("JOIN inbounds i ON i.id = ci.inbound_id").Where("rc.reseller_id = ? AND i.node_id IS NOT NULL", resellerID).Count(&attached).Error; err != nil {
		return err
	}
	if attached > 0 {
		return errors.New("reseller client uses a remote inbound whose traffic snapshots require verification and reconciliation")
	}
	return nil
}
