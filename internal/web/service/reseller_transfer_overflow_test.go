package service

import (
	"math"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestResellerImportRejectsOverflowBeforeCreatingClients(t *testing.T) {
	setupBulkDB(t)
	db := database.GetDB()
	inbound := model.Inbound{UserId: 1, Port: 19834, Tag: "transfer-overflow", Protocol: model.VLESS, Settings: `{"clients":[]}`}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	scope := &ClientTransferScope{Reseller: true, ResellerID: 1, InboundIDs: map[int]struct{}{inbound.Id: {}}}
	envelope := ClientTransferEnvelope{Format: clientTransferFormat, Version: clientTransferVersion}
	for _, email := range []string{"large-a@example.test", "large-b@example.test", "large-c@example.test"} {
		envelope.Clients = append(envelope.Clients, ClientTransferEntry{Client: model.Client{Email: email, TotalGB: math.MaxInt64 / 2}, InboundIds: []int{inbound.Id}})
	}
	svc := &ClientService{}
	preflight, err := svc.ValidateClientTransfer(&InboundService{}, envelope, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(preflight.Errors) == 0 || preflight.AdditionalQuota < 0 {
		t.Fatalf("overflow accepted: %+v", preflight)
	}
	report, err := svc.ImportClients(&InboundService{}, envelope, scope)
	if err != nil || !report.Preflight || report.Created != 0 {
		t.Fatalf("import was not rejected: %+v %v", report, err)
	}
	var count int64
	if err := db.Model(&model.ClientRecord{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("import created clients: %d %v", count, err)
	}
}
