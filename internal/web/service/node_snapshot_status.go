package service

import (
	"context"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
)

// NodeSnapshotSafetyStatus is an operational warning, not a proof of traffic
// completeness. Every registered node is currently included, even when its
// outbound global push is verified or it is disabled: the incoming node traffic
// snapshot itself is not authenticated and ordered yet.
type NodeSnapshotSafetyStatus struct {
	Available bool  `json:"available"`
	NodeCount int64 `json:"nodeCount"`
}

func readNodeSnapshotSafetyStatus(db *gorm.DB) NodeSnapshotSafetyStatus {
	var status NodeSnapshotSafetyStatus
	if db == nil {
		return status
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := db.WithContext(ctx).Model(&model.Node{}).Count(&status.NodeCount).Error; err != nil {
		return status
	}
	status.Available = true
	return status
}
