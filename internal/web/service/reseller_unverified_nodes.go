package service

import (
	"errors"

	"gorm.io/gorm"
)

// rejectResellerUnverifiedNodes blocks participating quota mutations for any
// reseller assigned a remote inbound or owning a client attached to one.
// Verified outbound global pushes do not authenticate the reverse node traffic
// snapshots. Even verified incoming snapshots do not prove historical or
// unpolled usage complete, so every remote node remains blocked. The
// caller must hold its normal allocation lock when mutation follows this check.
// This does not cover administrator/DB writes, missing associations or remote
// runtimes that have left no surviving association or baseline in these tables.
func rejectResellerUnverifiedNodes(tx *gorm.DB, resellerID int) error {
	if tx == nil || resellerID <= 0 {
		return errors.New("cannot verify reseller node accounting")
	}
	var assigned int64
	if err := tx.Table("reseller_inbounds AS ri").Joins("LEFT JOIN inbounds i ON i.id = ri.inbound_id").Where("ri.reseller_id = ? AND (i.id IS NULL OR i.node_id IS NOT NULL)", resellerID).Count(&assigned).Error; err != nil {
		return err
	}
	if assigned > 0 {
		return errors.New("reseller has a remote or missing inbound whose traffic requires verification and reconciliation")
	}
	var attached int64
	if err := tx.Table("reseller_clients AS rc").Joins("JOIN clients c ON LOWER(TRIM(c.email)) = LOWER(TRIM(rc.email))").Joins("JOIN client_inbounds ci ON ci.client_id = c.id").Joins("LEFT JOIN inbounds i ON i.id = ci.inbound_id").Where("rc.reseller_id = ? AND (i.id IS NULL OR i.node_id IS NOT NULL)", resellerID).Count(&attached).Error; err != nil {
		return err
	}
	if attached > 0 {
		return errors.New("reseller client uses a remote or missing inbound whose traffic requires verification and reconciliation")
	}
	// A durable node baseline is evidence of a past remote association even
	// if the inbound or client link has since disappeared. Do not interpret
	// the missing link as proof that the runtime stopped or final usage landed.
	var historical int64
	if err := tx.Table("reseller_clients AS rc").Joins("JOIN node_client_traffics b ON LOWER(TRIM(b.email)) = LOWER(TRIM(rc.email))").Where("rc.reseller_id = ?", resellerID).Count(&historical).Error; err != nil {
		return err
	}
	if historical > 0 {
		return errors.New("reseller client has a node traffic baseline requiring verification and reconciliation")
	}
	return nil
}
