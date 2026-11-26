package service

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestResellerGlobalDecreaseIsRetainedForManualReview(t *testing.T) {
	r, client := seedResellerRenewal(t)
	db := database.GetDB()
	svc := &InboundService{}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 400, Down: 500}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email}}); err == nil {
		t.Fatal("unversioned counter decrease was accepted")
	}
	var row model.ClientGlobalTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil || row.Up != 400 || row.Down != 500 {
		t.Fatalf("baseline changed: %+v %v", row, err)
	}
	var incident model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ?", -1).First(&incident).Error; err != nil || incident.SourceGuid != "master" || incident.PreviousUp != 400 || incident.PreviousDown != 500 || incident.ObservedUp != 0 || incident.ObservedDown != 0 {
		t.Fatalf("missing incident evidence: %+v %v", incident, err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&r, 1); err == nil {
		t.Fatal("quota allowed while incident requires reconciliation")
	}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 900, Down: 900}}); err == nil {
		t.Fatal("higher later snapshot cleared incident")
	}
}

func TestGlobalDecreaseMatchesNormalizedPriorIdentity(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	old := model.ClientGlobalTraffic{MasterGuid: "master", Email: " RENEW@EXAMPLE.TEST ", Up: 400, Down: 500}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email}}); err == nil {
		t.Fatal("decrease of normalized prior identity accepted")
	}
	var rows []model.ClientGlobalTraffic
	if err := db.Where("master_guid = ?", "master").Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].Id != old.Id || rows[0].Up != 400 || rows[0].Down != 500 {
		t.Fatalf("baseline changed: %+v %v", rows, err)
	}
	var incident model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ?", -1).First(&incident).Error; err != nil || incident.Email != client.Email || incident.PreviousUp != 400 {
		t.Fatalf("incident not recorded: %+v %v", incident, err)
	}
}

func TestGlobalSnapshotIncreaseUpdatesNormalizedPriorIdentity(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	old := model.ClientGlobalTraffic{MasterGuid: "master", Email: " RENEW@EXAMPLE.TEST ", Up: 400, Down: 500}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 450, Down: 500}}); err != nil {
		t.Fatal(err)
	}
	var rows []model.ClientGlobalTraffic
	if err := db.Where("master_guid = ?", "master").Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].Id != old.Id || rows[0].Up != 450 {
		t.Fatalf("prior row not updated: %+v %v", rows, err)
	}
}

func TestGlobalTrafficRejectsAmbiguousNormalizedPriorRows(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	for _, old := range []model.ClientGlobalTraffic{
		{MasterGuid: "master", Email: client.Email, Up: 400},
		{MasterGuid: "master", Email: " RENEW@EXAMPLE.TEST ", Up: 500},
	} {
		if err := db.Create(&old).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := (&InboundService{}).AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 1}}); err == nil {
		t.Fatal("ambiguous prior global counters accepted")
	}
	var rows []model.ClientGlobalTraffic
	if err := db.Where("master_guid = ?", "master").Order("id").Find(&rows).Error; err != nil || len(rows) != 2 || rows[0].Up != 400 || rows[1].Up != 500 {
		t.Fatalf("ambiguous rows changed: %+v %v", rows, err)
	}
	var count int64
	if err := db.Model(&model.ResellerTrafficArchive{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unexpected archived reset: %d %v", count, err)
	}
}

func TestGlobalTrafficRejectsCorruptNormalizedPriorRow(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	old := model.ClientGlobalTraffic{MasterGuid: "master", Email: " RENEW@EXAMPLE.TEST ", Up: -1}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 5}}); err == nil {
		t.Fatal("corrupt normalized prior counter accepted")
	}
	var rows []model.ClientGlobalTraffic
	if err := db.Where("master_guid = ?", "master").Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].Id != old.Id || rows[0].Up != -1 {
		t.Fatalf("corrupt row changed: %+v %v", rows, err)
	}
}

func TestGlobalDecreaseRetainsPendingEvidenceWhenDatabaseWriteFails(t *testing.T) {
	r, client := seedResellerRenewal(t)
	db := database.GetDB()
	svc := &InboundService{}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 400}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_global_incident BEFORE INSERT ON reseller_node_counter_incidents BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pendingGlobalCounterIncident.Lock()
		pendingGlobalCounterIncident.first = nil
		pendingGlobalCounterIncident.Unlock()
	})
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 1}}); err == nil || !hasPendingGlobalCounterIncident() {
		t.Fatal("failed incident write discarded pending observation")
	}
	if err := os.RemoveAll(counterIncidentDir()); err != nil {
		t.Fatal(err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&r, 1); err == nil {
		t.Fatal("pending in-memory observation did not block quota")
	}
	if err := db.Exec("DROP TRIGGER reject_global_incident").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 900}}); err == nil {
		t.Fatal("retry should report original incident")
	}
	if hasPendingGlobalCounterIncident() {
		t.Fatal("pending evidence not persisted on retry")
	}
	var incident model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ?", -1).First(&incident).Error; err != nil || incident.PreviousUp != 400 || incident.ObservedUp != 1 {
		t.Fatalf("original evidence lost: %+v %v", incident, err)
	}
}
