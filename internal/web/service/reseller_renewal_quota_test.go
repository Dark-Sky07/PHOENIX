package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

func TestResellerAutoRenewalChecksRemainingAllocation(t *testing.T) {
	for _, test := range []struct {
		limit     int64
		wantCount int64
	}{{799, 0}, {800, 1}, {0, 1}} {
		t.Run(fmt.Sprint(test.limit), func(t *testing.T) {
			r, client := seedResellerRenewal(t)
			db := database.GetDB()
			if err := db.Model(&r).Update("traffic_limit", test.limit).Error; err != nil {
				t.Fatal(err)
			}
			var count int64
			err := db.Transaction(func(tx *gorm.DB) error {
				var err error
				_, count, err = (&InboundService{}).autoRenewClients(tx)
				return err
			})
			if err != nil || count != test.wantCount {
				t.Fatalf("count=%d err=%v", count, err)
			}
			var row xray.ClientTraffic
			if err := db.Where("email = ?", client.Email).First(&row).Error; err != nil {
				t.Fatal(err)
			}
			if test.wantCount == 0 && (row.Up != 120 || row.Down != 180 || row.ExpiryTime > time.Now().UnixMilli()) {
				t.Fatalf("rejected renewal mutated traffic: %+v", row)
			}
			if test.wantCount == 1 && (row.Up != 0 || row.Down != 0 || row.ExpiryTime <= time.Now().UnixMilli()) {
				t.Fatalf("allowed renewal failed: %+v", row)
			}
		})
	}
}

func TestRenewalQuotaReservationsShareRemainingCapacity(t *testing.T) {
	r, first := seedResellerRenewal(t)
	db := database.GetDB()
	if err := db.Model(&r).Update("traffic_limit", 1600).Error; err != nil {
		t.Fatal(err)
	}
	other := model.ClientRecord{Email: "second-renew@example.test", TotalGB: 500, Enable: true}
	for _, v := range []any{&other, &model.ResellerClient{ResellerId: r.Id, Email: other.Email}, &xray.ClientTraffic{Email: other.Email, Up: 100, Down: 200}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	err := db.Transaction(func(tx *gorm.DB) error {
		tracker := renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}
		a, e := tracker.reserve(first.Email)
		if e != nil || !a {
			t.Fatalf("first reservation: %v %v", a, e)
		}
		b, e := tracker.reserve(other.Email)
		if e != nil || !b {
			t.Fatalf("second reservation: %v %v", b, e)
		}
		if tracker.accounts[r.Id].allocated != 1600 {
			t.Fatalf("allocation=%d", tracker.accounts[r.Id].allocated)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&r).Update("traffic_limit", 1599).Error; err != nil {
		t.Fatal(err)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		tracker := renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}
		a, e := tracker.reserve(first.Email)
		if e != nil || !a {
			t.Fatalf("first reservation: %v %v", a, e)
		}
		b, e := tracker.reserve(other.Email)
		if e != nil || b {
			t.Fatalf("second reservation should be denied: %v %v", b, e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
