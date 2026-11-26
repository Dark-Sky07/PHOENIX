package service

import (
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestGlobalTrafficWaitsForAllocationBeforeCommit(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	StartTrafficWriter()
	defer StopTrafficWriter()
	svc := &InboundService{}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 400, Down: 500}}); err != nil {
		t.Fatal(err)
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	started := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		close(started)
		finished <- svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 401, Down: 500}})
	}()
	<-started
	select {
	case err := <-finished:
		t.Fatalf("global snapshot crossed held allocation lock: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	var row model.ClientGlobalTraffic
	if err := database.GetDB().Where("email = ?", client.Email).First(&row).Error; err != nil || row.Up != 400 || row.Down != 500 {
		t.Fatalf("snapshot changed while locked: %+v %v", row, err)
	}
	unlock()
	unlock = nil
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("global snapshot did not resume after unlock")
	}
	stat, err := (&ResellerService{}).Stat(&reseller)
	if err != nil || stat.UsedTraffic != 901 {
		t.Fatalf("snapshot usage incorrect: %+v %v", stat, err)
	}
}

func TestGlobalTrafficReleasesAllocationAfterFailure(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	svc := &InboundService{}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 400}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_global_lock_archive BEFORE UPDATE ON client_global_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 401}}); err == nil {
		t.Fatal("expected archive failure")
	}
	if err := db.Exec("DROP TRIGGER reject_global_lock_archive").Error; err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- svc.AcceptGlobalTraffic("master", []*xray.ClientTraffic{{Email: client.Email, Up: 401}})
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("retry failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("allocation lock was retained after failure")
	}
}
