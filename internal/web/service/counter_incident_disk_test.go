package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestCounterIncidentFallbackSurvivesLostProcessMemory(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	decrease := &resellerNodeCounterDecrease{nodeID: 77, email: client.Email, previousUp: 120, previousDown: 180, observedUp: 2, observedDown: 180}
	rememberNodeCounterDecrease(decrease)
	t.Cleanup(func() {
		pendingNodeCounterIncidents.Lock()
		delete(pendingNodeCounterIncidents.byNode, 77)
		pendingNodeCounterIncidents.Unlock()
	})
	if err := persistPendingNodeCounterIncident(nil, 77); err == nil {
		t.Fatal("failed database write accepted")
	}
	entries, err := os.ReadDir(counterIncidentDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("files=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(counterIncidentDir(), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var evidence model.ResellerNodeCounterIncident
	if err := json.Unmarshal(data, &evidence); err != nil || evidence.Email != client.Email || evidence.PreviousUp != 120 || evidence.ObservedUp != 2 {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
	// Simulate loss of process-local memory after crash.
	pendingNodeCounterIncidents.Lock()
	delete(pendingNodeCounterIncidents.byNode, 77)
	pendingNodeCounterIncidents.Unlock()
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("fallback did not block allocation")
	}
	if _, err := (&InboundService{}).SetRemoteTraffic(77, &runtime.TrafficSnapshot{}, false); err == nil {
		t.Fatal("fallback did not block node sync")
	}
	if err := checkLocalXrayCounterIncidents(database.GetDB()); err == nil {
		t.Fatal("fallback did not block local traffic collection")
	}
	// Persisted fallback is intentionally not cleared automatically on DB recovery.
	if err := storeCounterIncidentFallback(evidence); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(counterIncidentDir(), entries[0].Name())); err != nil {
		t.Fatal(err)
	}
	if err := diskCounterIncidentError(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalXrayIncidentFallbackRecordsOnlyObservedDirection(t *testing.T) {
	_, client := seedResellerRenewal(t)
	state := xrayTrafficState{pendingCounterDecrease: &xray.UserTrafficCounterDecrease{Email: client.Email, Direction: "downlink", Previous: 180, Observed: 4}}
	if err := state.persistPendingCounterDecrease(nil); err == nil || state.pendingCounterDecrease == nil {
		t.Fatal("failed write forgot incident")
	}
	entries, err := os.ReadDir(counterIncidentDir())
	if err != nil || len(entries) != 1 {
		t.Fatalf("files=%v err=%v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(counterIncidentDir(), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var evidence model.ResellerNodeCounterIncident
	if err := json.Unmarshal(data, &evidence); err != nil || evidence.NodeId != 0 || evidence.Direction != "downlink" || evidence.PreviousDown != 180 || evidence.ObservedDown != 4 {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}
