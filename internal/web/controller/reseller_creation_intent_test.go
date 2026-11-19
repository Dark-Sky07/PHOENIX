package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestResellerImportIntentTracksSuccessAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failure=", fail), func(t *testing.T) {
			router := setupResellerInboundRouter(t)
			NewClientController(router.Group("/panel/api/clients"))
			db := database.GetDB()
			var reseller model.Reseller
			if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
				t.Fatal(err)
			}
			in := model.Inbound{UserId: 1, Port: 19678, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "import-intent-test"}
			if err := db.Create(&in).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: in.Id}).Error; err != nil {
				t.Fatal(err)
			}
			if fail {
				if err := db.Exec(`CREATE TRIGGER fail_import_inbound BEFORE UPDATE ON inbounds BEGIN SELECT RAISE(ABORT, 'injected import failure'); END`).Error; err != nil {
					t.Fatal(err)
				}
			}
			payload := map[string]any{"format": "phoenix-client-transfer", "version": 1, "clients": []any{map[string]any{"client": map[string]any{"email": "intent-import@example.test", "totalGB": 50, "enable": true}, "inboundIds": []int{in.Id}}}}
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/import", bytes.NewReader(data))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			var body resellerWriteContract
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Success == fail {
				t.Fatalf("unexpected import result: %s err=%v", w.Body.String(), err)
			}
			var intents []model.ResellerCreationIntent
			if err := db.Where("reseller_id = ?", reseller.Id).Find(&intents).Error; err != nil {
				t.Fatal(err)
			}
			if fail {
				if len(intents) != 1 || intents[0].Operation != "import" || !strings.Contains(intents[0].Details, "intent-import@example.test") {
					t.Fatalf("failed import not recorded: %+v", intents)
				}
				if err := (&service.ResellerService{}).CheckClientQuota(&reseller, 1, 50); err == nil {
					t.Fatal("failed import did not block quota")
				}
			} else {
				if len(intents) != 0 {
					t.Fatalf("successful import left intent: %+v", intents)
				}
				owned, err := (&service.ResellerService{}).OwnsClient(reseller.Id, "intent-import@example.test")
				if err != nil || !owned {
					t.Fatalf("import not mapped: %v %v", owned, err)
				}
			}
		})
	}
}

func TestCreationIssueReportShowsStatusWithoutLeakingSecrets(t *testing.T) {
	router := setupResellerInboundRouter(t)
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	intent := model.ResellerCreationIntent{Id: "issue-1", ResellerId: reseller.Id, Email: "issue@example.test", SubID: "sensitive-subscription-id", InboundIDs: "[1]", Details: `{"secret":"private-manifest"}`}
	if err := db.Create(&intent).Error; err != nil {
		t.Fatal(err)
	}
	// Exercise the actual admin report handler; access control is supplied by
	// the API router's reseller guard in production.
	NewResellerController(router.Group("/panel/api/resellers"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/panel/api/resellers/report/%d", reseller.Id), nil))
	body := w.Body.String()
	if !strings.Contains(body, `"pendingCreations":1`) || !strings.Contains(body, "issue@example.test") {
		t.Fatalf("missing admin issue status: %s", body)
	}
	if strings.Contains(body, "sensitive-subscription-id") || strings.Contains(body, "private-manifest") {
		t.Fatalf("admin report exposed intent secrets: %s", body)
	}
	// The self-report exposes the count, not the admin-only issue details.
	NewResellerSelfController(router.Group("/panel/api/reseller"))
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panel/api/reseller/report", nil))
	body = w.Body.String()
	if !strings.Contains(body, `"pendingCreations":1`) || strings.Contains(body, "creationIssues") || strings.Contains(body, "issue@example.test") {
		t.Fatalf("self report exposed issue details or omitted count: %s", body)
	}
}
