package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"math"
	"testing"
)

func TestResellerBulkAllocationRejectsOverflow(t *testing.T) {
	for _, tc := range []struct {
		count   int
		bytes   int64
		want    int64
		invalid bool
	}{
		{2, 50, 100, false},
		{1, math.MaxInt64, math.MaxInt64, false},
		{2, math.MaxInt64, 0, true},
		{2, math.MinInt64, 0, true},
		{-1, 1, 0, true},
	} {
		got, err := resellerBulkAllocation(tc.count, tc.bytes)
		if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
			t.Fatalf("count=%d bytes=%d: got %d, %v", tc.count, tc.bytes, got, err)
		}
	}
}

// The quota check and mapping write must be serialized across real requests:
// both requests would fit an empty reseller, but not both fit together.
func TestConcurrentResellerClientCreationRespectsTrafficLimit(t *testing.T) {
	router := setupResellerInboundRouter(t)
	NewClientController(router.Group("/panel/api/clients"))
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&reseller).Update("traffic_limit", 100).Error; err != nil {
		t.Fatal(err)
	}
	inbound := model.Inbound{UserId: 1, Port: 19321, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "concurrent-reseller"}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan resellerWriteContract, 2)
	for _, email := range []string{"quota-first@example.test", "quota-second@example.test"} {
		go func(email string) {
			payload, _ := json.Marshal(map[string]any{"client": map[string]any{"email": email, "totalGB": 60, "enable": true}, "inboundIds": []int{inbound.Id}})
			req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/add", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-start
			router.ServeHTTP(w, req)
			var result resellerWriteContract
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				results <- resellerWriteContract{Msg: "invalid JSON: " + w.Body.String()}
				return
			}
			results <- result
		}(email)
	}
	close(start)
	first, second := <-results, <-results
	if first.Success == second.Success {
		t.Fatalf("expected exactly one success, got %+v and %+v", first, second)
	}
	var mappings int64
	if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ?", reseller.Id).Count(&mappings).Error; err != nil || mappings != 1 {
		t.Fatalf("mappings=%d err=%v", mappings, err)
	}
	stat, err := (&service.ResellerService{}).Stat(&reseller)
	if err != nil || stat.AllocatedTraffic != 60 || stat.RemainingTraffic != 40 {
		t.Fatalf("unexpected quota after creation: %+v, %v", stat, err)
	}
}

func TestResellerBulkCreateRejectsOverflowAtMaximumLimit(t *testing.T) {
	setupResellerInboundRouter(t)
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	reseller.TrafficLimit = math.MaxInt64
	inbound := model.Inbound{UserId: 1, Port: 19322, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "overflow-reseller"}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	controller := &ClientController{}
	payloads := []service.ClientCreatePayload{
		{Client: model.Client{Email: "max-a@example.test", TotalGB: math.MaxInt64 / 2}, InboundIds: []int{inbound.Id}},
		{Client: model.Client{Email: "max-b@example.test", TotalGB: math.MaxInt64 / 2}, InboundIds: []int{inbound.Id}},
		{Client: model.Client{Email: "max-c@example.test", TotalGB: 2}, InboundIds: []int{inbound.Id}},
	}
	if err := controller.scopeResellerClientWrite(&reseller, payloads); err == nil || !strings.Contains(err.Error(), "combined client allocation") {
		t.Fatalf("expected overflow rejection, got %v", err)
	}
	var clients int64
	if err := db.Model(&model.ClientRecord{}).Count(&clients).Error; err != nil || clients != 0 {
		t.Fatalf("preflight mutated clients: %d %v", clients, err)
	}
}
