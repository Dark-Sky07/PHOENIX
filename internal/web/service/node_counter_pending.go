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
	if db == nil {
		return fmt.Errorf("%w; database unavailable while persisting node counter incident", decrease)
	}
	incident := model.ResellerNodeCounterIncident{NodeId: decrease.nodeID, Email: decrease.email, PreviousUp: decrease.previousUp, PreviousDown: decrease.previousDown, ObservedUp: decrease.observedUp, ObservedDown: decrease.observedDown}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error; err != nil {
		return fmt.Errorf("%w; could not persist counter incident: %v", decrease, err)
	}
	pendingNodeCounterIncidents.Lock()
	if pendingNodeCounterIncidents.byNode[nodeID] == decrease {
		delete(pendingNodeCounterIncidents.byNode, nodeID)
	}
	pendingNodeCounterIncidents.Unlock()
	return decrease
}

func pendingCounterIncidentError() error {
	if xrayCounterIncidentPending.Load() || hasPendingNodeCounterIncidents() {
		return errors.New("counter incident persistence pending; manual reconciliation required")
	}
	return nil
}
