package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// Exercises the real HTTP path rather than the compensation helper alone.
func TestResellerCreationFailureCompensatesGeneratedIdentity(t *testing.T) {
	for _, failure := range []string{"second-inbound", "sale-insert"} {
		t.Run(failure, func(t *testing.T) {
			router := setupResellerInboundRouter(t)
			NewClientController(router.Group("/panel/api/clients"))
			db := database.GetDB()
			var reseller model.Reseller
			if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
				t.Fatal(err)
			}
			inbound := model.Inbound{UserId: 1, Port: 19501, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "compensation-one"}
			if err := db.Create(&inbound).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
				t.Fatal(err)
			}
			ids := []int{inbound.Id}
			if failure == "second-inbound" {
				broken := model.Inbound{UserId: 1, Port: 19502, Protocol: model.VLESS, Settings: `{broken`, Tag: "compensation-broken"}
				if err := db.Create(&broken).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: broken.Id}).Error; err != nil {
					t.Fatal(err)
				}
				ids = append(ids, broken.Id)
			} else {
				if err := db.Exec("CREATE TRIGGER reject_sale_insert BEFORE INSERT ON reseller_sales BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			payload, _ := json.Marshal(map[string]any{"client": map[string]any{"email": "compensation@example.test", "totalGB": 50, "enable": true}, "inboundIds": ids})
			req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/add", bytes.NewReader(payload))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			var response resellerWriteContract
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Success {
				t.Fatalf("unexpected response %s %v", w.Body.String(), err)
			}
			for _, table := range []any{&model.ClientRecord{}, &model.ResellerClient{}, &model.ResellerSale{}, &xray.ClientTraffic{}} {
				var n int64
				if err := db.Model(table).Count(&n).Error; err != nil || n != 0 {
					t.Fatalf("partial persisted data %T count=%d error=%v; response=%s", table, n, err, w.Body.String())
				}
			}
		})
	}
}
