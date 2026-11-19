package service

import (
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestTransferRollbackNeverDeletesUnverifiedIdentity(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	other := model.ClientRecord{Email: "other@example.test", SubID: "someone-elses-id"}
	if err := db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ClientService{}
	err := rollbackTransferClients(svc, &InboundService{}, []transferCreatedIdentity{
		{email: other.Email, subID: other.SubID, generated: false},
	})
	if err == nil || !strings.Contains(err.Error(), "manually") {
		t.Fatalf("expected manual verification: %v", err)
	}
	err = rollbackTransferClients(svc, &InboundService{}, []transferCreatedIdentity{
		{email: other.Email, subID: "generated-other-id", generated: true},
	})
	if err == nil {
		t.Fatal("identity mismatch accepted")
	}
	var count int64
	if err := db.Model(&model.ClientRecord{}).Where("id = ?", other.Id).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("unrelated client deleted: %d %v", count, err)
	}
}
