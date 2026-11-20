package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
)

func TestPendingNodeCounterIncidentRetriesWithoutLosingEvidence(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	db := database.GetDB()
	decrease := &resellerNodeCounterDecrease{nodeID: 77, email: client.Email, previousUp: 120, previousDown: 180, observedUp: 2, observedDown: 180}
	rememberNodeCounterDecrease(decrease)
	t.Cleanup(func() {
		pendingNodeCounterIncidents.Lock()
		delete(pendingNodeCounterIncidents.byNode, 77)
		pendingNodeCounterIncidents.Unlock()
	})
	if err := persistPendingNodeCounterIncident(nil, 77); err == nil || !hasPendingNodeCounterIncidents() {
		t.Fatal("failed write forgot incident")
	}
	// A later higher reading must not replace the first observed reset.
	rememberNodeCounterDecrease(&resellerNodeCounterDecrease{nodeID: 77, email: client.Email, previousUp: 120, previousDown: 180, observedUp: 100, observedDown: 180})
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("allocation bypassed pending incident")
	}
	if _, err := (&ClientService{}).Delete(&InboundService{}, client.Id, false); err == nil {
		t.Fatal("removal bypassed pending incident")
	}
	// Even an empty or changed snapshot cannot run before the first evidence is stored.
	if _, err := (&InboundService{}).SetRemoteTraffic(77, &runtime.TrafficSnapshot{}, false); err == nil {
		t.Fatal("retry silently resumed sync")
	}
	if hasPendingNodeCounterIncidents() {
		t.Fatal("successful write did not clear memory barrier")
	}
	var incident model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ? AND email = ?", 77, client.Email).First(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if incident.PreviousUp != 120 || incident.ObservedUp != 2 {
		t.Fatalf("first evidence was replaced: %+v", incident)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("durable incident did not block allocation")
	}
}
