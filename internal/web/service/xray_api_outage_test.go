package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestUnreachableXrayAPIPersistsAccountingBarrierWithoutInventingTraffic(t *testing.T) {
	reseller, _ := seedResellerRenewal(t)
	db := database.GetDB()
	xrayAPIOutagePending.Store(false)
	t.Cleanup(func() { xrayAPIOutagePending.Store(false) })

	if err := recordUnreachableXrayAPI(db); err != nil {
		t.Fatal(err)
	}
	var incident model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ? AND direction = ?", 0, "api-unreachable").First(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if incident.Email != "" || incident.PreviousUp != 0 || incident.PreviousDown != 0 || incident.ObservedUp != 0 || incident.ObservedDown != 0 {
		t.Fatalf("API outage invented traffic evidence: %+v", incident)
	}
	if xrayAPIOutagePending.Load() {
		t.Fatal("durably stored API outage remained pending")
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("quota allocation was allowed after unreachable local Xray API")
	}
	if err := checkLocalXrayCounterIncidents(db); err == nil {
		t.Fatal("local traffic collector resumed before API outage reconciliation")
	}
}

func TestUnreachableXrayAPIStaysPendingWhenDatabaseIsUnavailable(t *testing.T) {
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	xrayAPIOutagePending.Store(false)
	t.Cleanup(func() { xrayAPIOutagePending.Store(false) })
	if err := recordUnreachableXrayAPI(nil); err == nil {
		t.Fatal("unrecorded API outage was accepted")
	}
	if !xrayAPIOutagePending.Load() {
		t.Fatal("failed outage persistence did not keep an in-memory barrier")
	}
}
