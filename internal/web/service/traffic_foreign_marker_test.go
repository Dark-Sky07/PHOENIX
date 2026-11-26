package service

import (
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestTrafficRecoveryDefersRenewalWithForeignPollMarker(t *testing.T) {
	_, client := seedResellerRenewal(t)
	unlock, err := LockResellerAllocationForXrayRecovery()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, id := range []string{"recovering-xray", "pending-ppp"} {
		if err := BeginXrayPoll(id); err != nil {
			t.Fatal(err)
		}
	}
	svc := &InboundService{}
	if _, _, err := svc.AddCheckedXrayTrafficBatchUnderAllocationLock("recovering-xray", nil, nil, []XrayTrafficIdentity{}); err != nil {
		t.Fatal(err)
	}
	var row xray.ClientTraffic
	if err := database.GetDB().Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 120 || row.Down != 180 || row.ExpiryTime > time.Now().UnixMilli() {
		t.Fatalf("renewed with unresolved foreign poll: %+v", row)
	}
	if err := FinishXrayPoll("pending-ppp"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddCheckedXrayTrafficBatchUnderAllocationLock("new-xray", nil, nil, []XrayTrafficIdentity{}); err != nil {
		t.Fatal(err)
	}
	// The first marker is still foreign to this batch, so remove it before a
	// subsequent ordinary poll can renew.
	if err := database.GetDB().Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 120 || row.Down != 180 {
		t.Fatalf("foreign Xray marker ignored: %+v", row)
	}
	if err := FinishXrayPoll("recovering-xray"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AddCheckedXrayTrafficBatchUnderAllocationLock("final-xray", nil, nil, []XrayTrafficIdentity{}); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Where("email = ?", client.Email).First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Up != 0 || row.Down != 0 || row.ExpiryTime <= time.Now().UnixMilli() {
		t.Fatalf("renewal did not resume: %+v", row)
	}
}
