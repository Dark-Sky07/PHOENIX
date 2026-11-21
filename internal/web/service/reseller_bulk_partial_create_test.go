package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestBulkCreateCompensatesPartialNewClients(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprintf("custom=%v", custom), func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			first := model.Inbound{UserId: 1, Port: 19001, Tag: "bulk-partial-1", Protocol: model.VLESS, Settings: `{"clients":[]}`}
			second := model.Inbound{UserId: 1, Port: 19002, Tag: "bulk-partial-2", Protocol: model.VLESS, Settings: `{"clients":[]}`}
			for _, in := range []*model.Inbound{&first, &second} {
				if err := db.Create(in).Error; err != nil {
					t.Fatal(err)
				}
			}
			trigger := fmt.Sprintf("CREATE TRIGGER reject_second_inbound BEFORE UPDATE ON inbounds WHEN OLD.id = %d BEGIN SELECT RAISE(ABORT, 'injected failure'); END", second.Id)
			if err := db.Exec(trigger).Error; err != nil {
				t.Fatal(err)
			}
			c := model.Client{Email: "partial-bulk@example.test", Enable: true, TotalGB: 100}
			if custom {
				c.SubID = "custom-subid"
			}
			result, _, err := (&ClientService{}).BulkCreate(&InboundService{}, []ClientCreatePayload{{Client: c, InboundIds: []int{first.Id, second.Id}}})
			if err != nil || result.Created != 0 || len(result.Skipped) != 1 {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			var count int64
			if err := db.Model(&model.ClientRecord{}).Where("email = ?", c.Email).Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if !custom && count != 0 {
				t.Fatalf("generated partial client remains: %d", count)
			}
			if custom && (count != 1 || !strings.Contains(result.Skipped[0].Reason, "manually")) {
				t.Fatalf("custom identity not flagged: count=%d result=%+v", count, result)
			}
		})
	}
}
