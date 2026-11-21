package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestResellerGlobalResetPreservesUsage(t *testing.T) {
	r, client := seedResellerRenewal(t)
	svc := &InboundService{}
	push := func(up, down int64) error {
		return svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: up, Down: down}})
	}
	if err := push(400, 500); err != nil {
		t.Fatal(err)
	}
	if err := push(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := push(0, 0); err != nil {
		t.Fatal(err)
	}
	stat, err := (&ResellerService{}).Stat(&r)
	if err != nil || stat.UsedTraffic != 900 || stat.AllocatedTraffic != 1100 {
		t.Fatalf("reset lost or duplicated usage: %+v %v", stat, err)
	}
}

func TestResellerGlobalResetRollsBackOnArchiveFailure(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	svc := &InboundService{}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 400, Down: 500}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_global_archive BEFORE INSERT ON reseller_traffic_archives BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 0, Down: 0}}); err == nil {
		t.Fatal("expected failure")
	}
	var row model.ClientGlobalTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 400 || row.Down != 500 {
		t.Fatalf("global reset was committed after archive failure: %+v", row)
	}
}

func TestResellerGlobalResetMultipleMasters(t *testing.T) {
	r, client := seedResellerRenewal(t)
	svc := &InboundService{}
	push := func(master string, up, down int64) {
		t.Helper()
		if err := svc.AcceptGlobalTraffic(master, []*xray.ClientTraffic{{Email: client.Email, Up: up, Down: down}}); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		stat, err := (&ResellerService{}).Stat(&r)
		if err != nil || stat.UsedTraffic != 900 {
			t.Fatalf("usage after master reset: %+v %v", stat, err)
		}
	}
	push("master-a", 400, 500)
	push("master-b", 350, 450)
	check()
	push("master-a", 0, 0)
	check()
	push("master-b", 0, 0)
	check()
	push("master-a", 0, 0)
	check()
}

func TestResellerGlobalResetMatchesPriorNormalizedEmail(t *testing.T) {
	r, client := seedResellerRenewal(t)
	db := database.GetDB()
	old := model.ClientGlobalTraffic{MasterGuid: "master", Email: " RENEW@EXAMPLE.TEST ", Up: 400, Down: 500}
	if err := db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email}}); err != nil {
		t.Fatal(err)
	}
	var rows []model.ClientGlobalTraffic
	if err := db.Where("master_guid = ?", "master").Find(&rows).Error; err != nil || len(rows) != 1 || rows[0].Id != old.Id || rows[0].Up != 0 || rows[0].Down != 0 {
		t.Fatalf("global row not updated in place: %+v %v", rows, err)
	}
	stat, err := (&ResellerService{}).Stat(&r)
	if err != nil || stat.UsedTraffic != 900 {
		t.Fatalf("reset did not preserve usage: %+v %v", stat, err)
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
