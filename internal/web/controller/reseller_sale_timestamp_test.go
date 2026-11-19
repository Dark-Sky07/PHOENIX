package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestResellerCannotBackdateLiveSaleViaClientMetadata(t *testing.T) {
	router := setupResellerInboundRouter(t)
	NewClientController(router.Group("/panel/api/clients"))
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	inbound := model.Inbound{UserId: 1, Port: 19550, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "sale-timestamp"}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	before := time.Now().UnixMilli()
	payload, _ := json.Marshal(map[string]any{"client": map[string]any{"email": "backdate@example.test", "totalGB": 50, "enable": true, "createdAt": int64(946684800000)}, "inboundIds": []int{inbound.Id}})
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/add", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	after := time.Now().UnixMilli()
	var result resellerWriteContract
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.Success {
		t.Fatalf("create failed: %s %v", w.Body.String(), err)
	}
	var sale model.ResellerSale
	if err := db.Where("reseller_id = ? AND email = ?", reseller.Id, "backdate@example.test").First(&sale).Error; err != nil {
		t.Fatal(err)
	}
	if sale.CreatedAt < before || sale.CreatedAt > after {
		t.Fatalf("sale timestamp %d is outside server request [%d,%d]", sale.CreatedAt, before, after)
	}
}
