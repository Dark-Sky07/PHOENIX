package service

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
)

// CounterIncidentStatus is a panel-wide operational summary, not proof that
// traffic is complete. It contains no client identities or counter values.
type CounterIncidentStatus struct {
	Available        bool  `json:"available"`
	DatabaseCount    int64 `json:"databaseCount"`
	FallbackPresent  bool  `json:"fallbackPresent"`
	PendingInProcess bool  `json:"pendingInProcess"`
}

func readCounterIncidentStatus(db *gorm.DB) CounterIncidentStatus {
	status := CounterIncidentStatus{PendingInProcess: xrayCounterIncidentPending.Load() || xrayCoreExitPending.Load() || hasPendingXrayExitObservation() || hasPendingNodeCounterIncidents() || hasPendingGlobalCounterIncident()}
	entries, err := os.ReadDir(counterIncidentDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return status
	}
	status.FallbackPresent = len(entries) > 0
	journalPresent, err := xrayJournalPresent()
	if err != nil {
		return status
	}
	status.FallbackPresent = status.FallbackPresent || journalPresent
	if err := ObserveL2TPAccountingIncident(); err != nil {
		status.FallbackPresent = true
	}
	if db == nil {
		return status
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := db.WithContext(ctx).Model(&model.ResellerNodeCounterIncident{}).Count(&status.DatabaseCount).Error; err != nil {
		return status
	}
	var polls int64
	if err := db.WithContext(ctx).Model(&model.XrayTrafficInFlight{}).Count(&polls).Error; err != nil {
		return status
	}
	status.DatabaseCount += polls
	status.Available = true
	return status
}
