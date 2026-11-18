package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestResellerManualCounterCorrectionsPreserveUsage(t *testing.T) {
	r, client := seedResellerRenewal(t)
	db := database.GetDB()
	inbound := &InboundService{}
	check := func(wantUsed, wantAllocated int64) {
		t.Helper()
		stat, err := (&ResellerService{}).Stat(&r)
		if err != nil || stat.UsedTraffic != wantUsed || stat.AllocatedTraffic != wantAllocated {
			t.Fatalf("got %+v %v, want used=%d allocated=%d", stat, err, wantUsed, wantAllocated)
		}
	}
	if err := inbound.UpdateClientTrafficByEmail(client.Email, 60, 40); err != nil {
		t.Fatal(err)
	}
	check(300, 700)
	if err := inbound.UpdateClientTrafficByEmail(client.Email, 60, 40); err != nil {
		t.Fatal(err)
	}
	check(300, 700)
	if err := inbound.UpdateClientTrafficByEmail(client.Email, 160, 140); err != nil {
		t.Fatal(err)
	}
	check(500, 700)
	if err := inbound.ResetClientTrafficByEmail(client.Email); err != nil {
		t.Fatal(err)
	}
	check(500, 1000)
	if err := inbound.UpdateClientTrafficByEmail(client.Email, 10, 20); err != nil {
		t.Fatal(err)
	}
	check(530, 1000)
	if err := inbound.UpdateClientTrafficByEmail(client.Email, -1, 0); err == nil {
		t.Fatal("negative counters accepted")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 10 || row.Down != 20 {
		t.Fatalf("negative rewrite changed counters: %+v", row)
	}
}

func TestResellerCounterCorrectionHonorsGlobalOverlay(t *testing.T) {
	r, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "master", Email: client.Email, Up: 200, Down: 220}).Error; err != nil {
		t.Fatal(err)
	}
	svc := &InboundService{}
	if err := svc.UpdateClientTrafficByEmail(client.Email, 30, 40); err != nil {
		t.Fatal(err)
	}
	stat, err := (&ResellerService{}).Stat(&r)
	if err != nil || stat.UsedTraffic != 420 || stat.AllocatedTraffic != 500 {
		t.Fatalf("global overlay double-counted: %+v %v", stat, err)
	}
	if err := svc.ResetClientTrafficByEmail(client.Email); err != nil {
		t.Fatal(err)
	}
	stat, err = (&ResellerService{}).Stat(&r)
	if err != nil || stat.UsedTraffic != 420 || stat.AllocatedTraffic != 920 {
		t.Fatalf("global usage lost after reset: %+v %v", stat, err)
	}
}

func TestResellerManualCorrectionArchiveFailureRollsBack(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_rewrite_archive BEFORE INSERT ON reseller_traffic_archives BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := (&InboundService{}).UpdateClientTrafficByEmail(client.Email, 0, 0); err == nil {
		t.Fatal("expected archive failure")
	}
	var row xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 120 || row.Down != 180 {
		t.Fatalf("rewrite proceeded after archive failure: %+v", row)
	}
}
