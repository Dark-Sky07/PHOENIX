package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

// Runtime removal precedes persistence. Even when saving the inbound fails,
// callers must retain the restart signal rather than leaving runtime and DB
// potentially inconsistent.
func TestDeletePropagatesRestartOnInboundSaveFailure(t *testing.T) {
	for _, mode := range []string{"direct", "scoped", "scoped-bulk"} {
		t.Run(mode, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			client := model.ClientRecord{Email: "restart-delete@example.test", Enable: true}
			inbound := model.Inbound{UserId: 1, Port: 19589, Protocol: model.VLESS, Tag: "restart-delete", Settings: `{"clients":[{"email":"restart-delete@example.test","id":"abc","enable":true}]}`}
			for _, v := range []any{&client, &inbound} {
				if err := db.Create(v).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: inbound.Id}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("CREATE TRIGGER reject_inbound_save BEFORE UPDATE ON inbounds BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
				t.Fatal(err)
			}
			svc := &ClientService{}
			var restart bool
			var err error
			switch mode {
			case "direct":
				restart, err = svc.Delete(&InboundService{}, client.Id, false)
			case "scoped":
				restart, err = svc.DeleteByEmailForInbounds(&InboundService{}, client.Email, false, []int{inbound.Id})
			case "scoped-bulk":
				var result BulkDeleteResult
				result, restart, err = svc.BulkDeleteForInbounds(&InboundService{}, []string{client.Email}, false, []int{inbound.Id})
				if err == nil && len(result.Skipped) != 1 {
					t.Fatalf("expected skipped client: %+v", result)
				}
			}
			if mode != "scoped-bulk" && err == nil {
				t.Fatal("expected injected failure")
			}
			if !restart {
				t.Fatal("lost restart indication after failed deletion")
			}
		})
	}
}
