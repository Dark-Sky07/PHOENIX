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
