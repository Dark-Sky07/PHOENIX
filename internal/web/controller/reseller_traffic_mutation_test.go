package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestResellerCannotResetOrRewriteTrafficCounters(t *testing.T) {
	r := setupResellerInboundRouter(t)
	NewClientController(r.Group("/panel/api/clients"))
	for _, path := range []string{
		"/resetAllTraffics", "/bulkResetTraffic", "/resetTraffic/example", "/updateTraffic/example",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/panel/api/clients"+path, nil))
		var body resellerWriteContract
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid response: %v", path, err)
		}
		if w.Code != http.StatusForbidden || body.Success || body.Msg != errResellerTrafficResetUnavailable {
			t.Errorf("%s: unexpected response %d %+v", path, w.Code, body)
		}
	}
}

func TestResellerRemovalWaitsForTrafficReconciliation(t *testing.T) {
	r := setupResellerInboundRouter(t)
	NewClientController(r.Group("/panel/api/clients"))
	for _, path := range []string{"/del/client@example.test", "/client@example.test/detach", "/bulkDel", "/bulkDetach", "/delDepleted"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/panel/api/clients"+path, nil))
		var body resellerWriteContract
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != http.StatusForbidden || body.Success || body.Msg != errResellerUnverifiedRemoval {
			t.Errorf("%s: status %d body %s err %v", path, w.Code, w.Body.String(), err)
		}
	}
}

func TestResellerQuotaReductionRequiresReconciliation(t *testing.T) {
	setupResellerInboundRouter(t)
	db := database.GetDB()
	var r model.Reseller
	if err := db.Where("username = ?", "sara").First(&r).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&r).Update("traffic_limit", int64(1000)).Error; err != nil {
		t.Fatal(err)
	}
	r.TrafficLimit = 1000
	client := model.ClientRecord{Email: "reduce@example.test", TotalGB: 100, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	controller := &ClientController{}
	if err := controller.checkResellerClientQuotaDelta(&r, client.Email, 99); err == nil {
		t.Fatal("quota decrease accepted")
	}
	if err := controller.checkResellerClientQuotaDelta(&r, client.Email, 100); err != nil {
		t.Fatalf("unchanged quota rejected: %v", err)
	}
}

func TestResellerBlockedRemovalAndQuotaDecreaseLeaveOwnershipIntact(t *testing.T) {
	r := setupResellerInboundRouter(t)
	NewClientController(r.Group("/panel/api/clients"))
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	client := model.ClientRecord{Email: "protected@example.test", UUID: "protected-id", TotalGB: 100, Enable: true}
	inbound := model.Inbound{UserId: 1, Port: 19591, Protocol: model.VLESS, Tag: "protected", Settings: `{"clients":[{"email":"protected@example.test","id":"protected-id","totalGB":100,"enable":true}]}`}
	for _, row := range []any{&client, &inbound} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []any{&model.ClientInbound{ClientId: client.Id, InboundId: inbound.Id}, &model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}, &model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/panel/api/clients/del/"+client.Email, nil),
		httptest.NewRequest(http.MethodPost, "/panel/api/clients/update/"+client.Email, strings.NewReader(`{"email":"protected@example.test","totalGB":50,"enable":true}`)),
	} {
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var body resellerWriteContract
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Success {
			t.Fatalf("unexpected response: %s %v", w.Body.String(), err)
		}
	}
	var count int64
	if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ? AND email = ?", reseller.Id, client.Email).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("mapping changed: %d %v", count, err)
	}
	if err := db.First(&client, client.Id).Error; err != nil || client.TotalGB != 100 {
		t.Fatalf("client changed: %+v %v", client, err)
	}
}
