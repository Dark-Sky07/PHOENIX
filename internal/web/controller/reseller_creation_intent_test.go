package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
