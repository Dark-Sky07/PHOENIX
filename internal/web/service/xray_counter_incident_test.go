package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestLocalXrayCounterIncidentBlocksQuotaAndRemoval(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	db := database.GetDB()
	decrease := &xray.UserTrafficCounterDecrease{Email: client.Email, Direction: "downlink", Previous: 180, Observed: 12}
	if err := recordLocalXrayCounterIncident(db, decrease); err != nil {
		t.Fatal(err)
	}
	if err := recordLocalXrayCounterIncident(db, decrease); err != nil {
		t.Fatal(err)
	}
	var rows []model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ?", 0).Find(&rows).Error; err != nil || len(rows) != 1 {
		t.Fatalf("incidents=%+v err=%v", rows, err)
	}
	if rows[0].Direction != "downlink" || rows[0].PreviousDown != 180 || rows[0].ObservedDown != 12 {
		t.Fatalf("incident evidence=%+v", rows[0])
	}
	if err := checkLocalXrayCounterIncidents(db); err == nil {
		t.Fatal("local collector resumed with incident")
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("reseller allocation allowed")
	}
	if _, err := (&ClientService{}).Delete(&InboundService{}, client.Id, false); err == nil {
		t.Fatal("client removal allowed")
	}
	if err := db.Delete(&rows[0]).Error; err != nil {
		t.Fatal(err)
	}
	if err := checkLocalXrayCounterIncidents(db); err != nil {
		t.Fatal(err)
	}
}
