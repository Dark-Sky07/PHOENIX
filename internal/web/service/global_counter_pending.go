package service

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The unversioned upstream protocol cannot distinguish a reset from an old
// replay. Retain the first observation until it is durable; never infer bytes.
type globalCounterDecrease struct {
	masterGuid, email                                  string
	previousUp, previousDown, observedUp, observedDown int64
}

func (d *globalCounterDecrease) Error() string {
	return "upstream global counter decreased; manual reconciliation required"
}

var pendingGlobalCounterIncident = struct {
	sync.Mutex
	first *globalCounterDecrease
}{}

func rememberGlobalCounterIncident(d *globalCounterDecrease) {
	pendingGlobalCounterIncident.Lock()
	defer pendingGlobalCounterIncident.Unlock()
	if pendingGlobalCounterIncident.first == nil {
		copy := *d
		pendingGlobalCounterIncident.first = &copy
	}
}

func hasPendingGlobalCounterIncident() bool {
	pendingGlobalCounterIncident.Lock()
	defer pendingGlobalCounterIncident.Unlock()
	return pendingGlobalCounterIncident.first != nil
}

// Called under the traffic writer and allocation lock. The disk fallback is
// written first; if both stores fail, retain an in-process barrier for retry.
func retryPendingGlobalCounterIncident(db *gorm.DB) error {
	pendingGlobalCounterIncident.Lock()
	d := pendingGlobalCounterIncident.first
	pendingGlobalCounterIncident.Unlock()
	if d == nil {
		return nil
	}
	incident := model.ResellerNodeCounterIncident{NodeId: -1, Direction: "global-decreased", SourceGuid: d.masterGuid, Email: d.email, PreviousUp: d.previousUp, PreviousDown: d.previousDown, ObservedUp: d.observedUp, ObservedDown: d.observedDown}
	diskErr := storeCounterIncidentFallback(incident)
	var dbErr error
	if db == nil {
		dbErr = errors.New("database unavailable")
	} else {
		dbErr = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error
	}
	if dbErr != nil {
		return fmt.Errorf("%w; incident database error: %v; fallback error: %v", d, dbErr, diskErr)
	}
	pendingGlobalCounterIncident.Lock()
	if pendingGlobalCounterIncident.first == d {
		pendingGlobalCounterIncident.first = nil
	}
	pendingGlobalCounterIncident.Unlock()
	if diskErr != nil {
		return fmt.Errorf("%w; database incident retained; fallback error: %v", d, diskErr)
	}
	return d
}
