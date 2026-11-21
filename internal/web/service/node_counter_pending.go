package service

import (
	"errors"
	"fmt"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// The first observation for each node stays in memory until its marker is
// durable. This barrier is only local to the panel process.
var pendingNodeCounterIncidents = struct {
	sync.Mutex
	byNode map[int]*resellerNodeCounterDecrease
}{byNode: make(map[int]*resellerNodeCounterDecrease)}

func rememberNodeCounterDecrease(decrease *resellerNodeCounterDecrease) {
	pendingNodeCounterIncidents.Lock()
	defer pendingNodeCounterIncidents.Unlock()
	if pendingNodeCounterIncidents.byNode[decrease.nodeID] == nil {
		saved := *decrease
		pendingNodeCounterIncidents.byNode[decrease.nodeID] = &saved
	}
}

func hasPendingNodeCounterIncidents() bool {
	pendingNodeCounterIncidents.Lock()
	defer pendingNodeCounterIncidents.Unlock()
	return len(pendingNodeCounterIncidents.byNode) > 0
}

// The traffic writer serializes calls for a node. Do not hold the memory lock
// while writing to the database; quota checks can still observe the barrier.
func persistPendingNodeCounterIncident(db *gorm.DB, nodeID int) error {
	pendingNodeCounterIncidents.Lock()
	decrease := pendingNodeCounterIncidents.byNode[nodeID]
	pendingNodeCounterIncidents.Unlock()
	if decrease == nil {
		return nil
	}
	incident := model.ResellerNodeCounterIncident{NodeId: decrease.nodeID, Email: decrease.email, PreviousUp: decrease.previousUp, PreviousDown: decrease.previousDown, ObservedUp: decrease.observedUp, ObservedDown: decrease.observedDown}
	// Make the local barrier durable before the database write when possible.
	// If either storage succeeds, retain that independent evidence for review.
	diskErr := storeCounterIncidentFallback(incident)
	var storeErr error
	if db == nil {
		storeErr = errors.New("database unavailable")
	} else {
		storeErr = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error
	}
	if storeErr != nil {
		if diskErr != nil {
			return fmt.Errorf("%w; database incident write failed: %v; fallback failed: %v", decrease, storeErr, diskErr)
		}
		return fmt.Errorf("%w; database incident write failed: %v; fallback retained for manual review", decrease, storeErr)
	}
	pendingNodeCounterIncidents.Lock()
	if pendingNodeCounterIncidents.byNode[nodeID] == decrease {
		delete(pendingNodeCounterIncidents.byNode, nodeID)
	}
	pendingNodeCounterIncidents.Unlock()
	if diskErr != nil {
		return fmt.Errorf("%w; fallback failed: %v; database incident retained", decrease, diskErr)
	}
	return decrease
}

func pendingCounterIncidentError() error {
	if xrayCounterIncidentPending.Load() || xrayCoreExitPending.Load() || hasPendingXrayExitObservation() || hasPendingNodeCounterIncidents() || hasPendingGlobalCounterIncident() {
		return errors.New("counter incident persistence pending; manual reconciliation required")
	}
	return diskCounterIncidentError()
}
