package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

func TestPendingCreationBlocksFurtherResellerAllocation(t *testing.T) {
	router := setupResellerInboundRouter(t)
	NewClientController(router.Group("/panel/api/clients"))
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	intent := model.ResellerCreationIntent{Id: "abandoned", ResellerId: reseller.Id, Email: "partial@example.test", SubID: "partial-id", RequestedBytes: 60, InboundIDs: "[1]"}
	if err := db.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&service.ResellerService{}).CheckClientQuota(&reseller, 1, 1); err == nil {
		t.Fatal("pending creation did not block quota check")
	}
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/add", bytes.NewBufferString(`{"client":{"email":"other@example.test","totalGB":1},"inboundIds":[1]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("pending creation should block mutation: %d %s", w.Code, w.Body.String())
	}
	var n int64
	if err := db.Model(&model.ClientRecord{}).Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("created client while intent pending: %d %v", n, err)
	}
}

func TestSuccessfulResellerCreationClearsIntentWithSale(t *testing.T) {
	router := setupResellerInboundRouter(t)
	NewClientController(router.Group("/panel/api/clients"))
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	inbound := model.Inbound{UserId: 1, Port: 19675, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "intent-test"}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"client": map[string]any{"email": "intent@example.test", "totalGB": 60, "enable": true}, "inboundIds": []int{inbound.Id}})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/add", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body resellerWriteContract
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Success {
		t.Fatalf("create failed: %s %v", w.Body.String(), err)
	}
	for _, tc := range []struct {
		table any
		want  int64
	}{{&model.ResellerCreationIntent{}, 0}, {&model.ResellerClient{}, 1}, {&model.ResellerSale{}, 1}} {
		var n int64
		if err := db.Model(tc.table).Count(&n).Error; err != nil || n != tc.want {
			t.Fatalf("%T count=%d want=%d err=%v", tc.table, n, tc.want, err)
		}
	}
}

func TestResellerBulkCreationRetainsIntentOnPartialFailure(t *testing.T) {
	router := setupResellerInboundRouter(t)
	NewClientController(router.Group("/panel/api/clients"))
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	inbound := model.Inbound{UserId: 1, Port: 19676, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "batch-intent-test"}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	// The malformed second client is skipped even though the first succeeds.
	data, _ := json.Marshal([]any{
		map[string]any{"client": map[string]any{"email": "batch-ok@example.test", "totalGB": 50}, "inboundIds": []int{inbound.Id}},
		map[string]any{"client": map[string]any{"email": "bad email", "totalGB": 50}, "inboundIds": []int{inbound.Id}},
	})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/bulkCreate", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body resellerWriteContract
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Success {
		t.Fatalf("partial batch should need review: %s %v", w.Body.String(), err)
	}
	var intents []model.ResellerCreationIntent
	if err := db.Where("reseller_id = ?", reseller.Id).Find(&intents).Error; err != nil {
		t.Fatal(err)
	}
	if len(intents) != 1 || intents[0].Operation != "bulk_create" || !strings.Contains(intents[0].Details, "batch-ok@example.test") {
		t.Fatalf("missing batch intent: %+v", intents)
	}
	var mappings int64
	if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ?", reseller.Id).Count(&mappings).Error; err != nil || mappings != 1 {
		t.Fatalf("successful client mapping lost: count=%d err=%v", mappings, err)
	}
	if err := (&service.ResellerService{}).CheckClientQuota(&reseller, 1, 1); err == nil {
		t.Fatal("pending batch did not block allocation")
	}
}

func TestResellerBulkCreationClearsIntentAfterSuccess(t *testing.T) {
	router := setupResellerInboundRouter(t)
	NewClientController(router.Group("/panel/api/clients"))
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	inbound := model.Inbound{UserId: 1, Port: 19677, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "batch-intent-success"}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal([]any{map[string]any{"client": map[string]any{"email": "batch-success@example.test", "totalGB": 50}, "inboundIds": []int{inbound.Id}}})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/bulkCreate", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var body resellerWriteContract
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Success {
		t.Fatalf("batch failed: %s %v", w.Body.String(), err)
	}
	var count int64
	if err := db.Model(&model.ResellerCreationIntent{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("intent count=%d err=%v", count, err)
	}
	if err := db.Model(&model.ResellerSale{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("sale count=%d err=%v", count, err)
	}
}
