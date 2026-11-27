package service

import (
	"encoding/json"
	"os"
	"os/exec"
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
	// A remote-node fallback still blocks the affected allocation paths above,
	// but must not freeze the unrelated local Xray collector after a restart.
	if err := checkLocalXrayCounterIncidents(database.GetDB()); err != nil {
		t.Fatalf("remote fallback blocked local traffic collection: %v", err)
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

func TestCounterIncidentFallbackStoredBeforeSuccessfulDatabaseWrite(t *testing.T) {
	for _, kind := range []string{"node", "xray"} {
		t.Run(kind, func(t *testing.T) {
			reseller, client := seedResellerRenewal(t)
			db := database.GetDB()
			if kind == "node" {
				rememberNodeCounterDecrease(&resellerNodeCounterDecrease{nodeID: 77, email: client.Email, previousUp: 120, previousDown: 180, observedUp: 2, observedDown: 180})
				t.Cleanup(func() {
					pendingNodeCounterIncidents.Lock()
					delete(pendingNodeCounterIncidents.byNode, 77)
					pendingNodeCounterIncidents.Unlock()
				})
				if err := persistPendingNodeCounterIncident(db, 77); err == nil {
					t.Fatal("incident not reported")
				}
			} else {
				state := xrayTrafficState{pendingCounterDecrease: &xray.UserTrafficCounterDecrease{Email: client.Email, Direction: "uplink", Previous: 120, Observed: 2}}
				if err := state.persistPendingCounterDecrease(db); err == nil || state.pendingCounterDecrease != nil {
					t.Fatal("incident not stored and reported")
				}
			}
			entries, err := os.ReadDir(counterIncidentDir())
			if err != nil || len(entries) != 1 {
				t.Fatalf("fallback=%v err=%v", entries, err)
			}
			var n int64
			if err := db.Model(&model.ResellerNodeCounterIncident{}).Count(&n).Error; err != nil || n != 1 {
				t.Fatalf("database marker count=%d err=%v", n, err)
			}
			// Removing the database marker alone cannot clear the fallback barrier.
			if err := db.Where("1 = 1").Delete(&model.ResellerNodeCounterIncident{}).Error; err != nil {
				t.Fatal(err)
			}
			if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
				t.Fatal("fallback bypassed after DB marker removal")
			}
		})
	}
}

// Each helper is a fresh OS process. The writer exits without Go cleanup to
// model an abrupt panel exit after the fallback has reported durable success.
func TestCounterIncidentFallbackAcrossProcesses(t *testing.T) {
	mode := os.Getenv("PHOENIX_COUNTER_INCIDENT_HELPER")
	if mode != "" {
		switch mode {
		case "write":
			incident := model.ResellerNodeCounterIncident{NodeId: 77, Email: "renew@example.test", PreviousUp: 120, PreviousDown: 180, ObservedUp: 2, ObservedDown: 180}
			if err := storeCounterIncidentFallback(incident); err != nil {
				t.Fatalf("store: %v", err)
			}
			os.Exit(0)
		case "check":
			if err := pendingCounterIncidentError(); err == nil {
				t.Fatal("fresh process ignored the fallback")
			}
			return
		default:
			t.Fatalf("unknown helper mode %q", mode)
		}
	}
	reseller, _ := seedResellerRenewal(t)
	for _, mode := range []string{"write", "check"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCounterIncidentFallbackAcrossProcesses$")
		cmd.Env = append(os.Environ(), "PHOENIX_COUNTER_INCIDENT_HELPER="+mode)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper %s: %v\n%s", mode, err, output)
		}
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("quota ignored marker written by another process")
	}
	var n int64
	if err := database.GetDB().Model(&model.ResellerNodeCounterIncident{}).Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("test unexpectedly relied on database incident: count=%d err=%v", n, err)
	}
}
