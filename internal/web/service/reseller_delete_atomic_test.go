package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestResellerDeleteDatabaseCleanupRollsBackTogether(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		name := "single"
		if bulk {
			name = "bulk"
		}
		t.Run(name, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			r := model.Reseller{Username: "delete-rollback", Password: "hash", Enable: true}
			c := model.ClientRecord{Email: "delete-rollback@example.test", TotalGB: 500, Enable: true}
			for _, v := range []any{&r, &c} {
				if err := db.Create(v).Error; err != nil {
					t.Fatal(err)
				}
			}
			for _, v := range []any{&model.ResellerClient{ResellerId: r.Id, Email: c.Email}, &xray.ClientTraffic{Email: c.Email, Up: 120, Down: 180}} {
				if err := db.Create(v).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Exec("CREATE TRIGGER reject_client_delete BEFORE DELETE ON clients BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
				t.Fatal(err)
			}
			svc := &ClientService{}
			var err error
			if bulk {
				_, _, err = svc.BulkDelete(&InboundService{}, []string{c.Email}, false)
			} else {
				_, err = svc.Delete(&InboundService{}, c.Id, false)
			}
			if err == nil {
				t.Fatal("expected injected failure")
			}
			var clients, mappings, traffics, archives int64
			for _, q := range []struct {
				model any
				count *int64
			}{{&model.ClientRecord{}, &clients}, {&model.ResellerClient{}, &mappings}, {&xray.ClientTraffic{}, &traffics}, {&model.ResellerTrafficArchive{}, &archives}} {
				if err := db.Model(q.model).Count(q.count).Error; err != nil {
					t.Fatal(err)
				}
			}
			if clients != 1 || mappings != 1 || traffics != 1 || archives != 0 {
				t.Fatalf("partial delete: clients=%d mappings=%d traffic=%d archives=%d", clients, mappings, traffics, archives)
			}
		})
	}
}
