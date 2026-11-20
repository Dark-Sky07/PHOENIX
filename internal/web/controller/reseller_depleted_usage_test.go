package controller

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestResellerDeleteDepletedUsesEffectiveUsageWithoutOverflow(t *testing.T) {
	router := setupResellerInboundRouter(t)
	NewClientController(router.Group("/panel/api/clients"))
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	reseller.TrafficLimit = math.MaxInt64
	if err := db.Model(&reseller).Update("traffic_limit", reseller.TrafficLimit).Error; err != nil {
		t.Fatal(err)
	}
	clients := []model.Client{
		{Email: "global-only@example.test", ID: "global-id", TotalGB: 100, Enable: true},
		{Email: "overflow@example.test", ID: "overflow-id", TotalGB: math.MaxInt64, Enable: true},
		{Email: "remaining@example.test", ID: "remaining-id", TotalGB: 100, Enable: true},
	}
	settings, err := json.Marshal(map[string]any{"clients": clients})
	if err != nil {
		t.Fatal(err)
	}
	inbound := model.Inbound{UserId: 1, Port: 19567, Protocol: model.VLESS, Tag: "depleted-test", Settings: string(settings)}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		record := model.ClientRecord{Email: c.Email, UUID: c.ID, TotalGB: c.TotalGB, Enable: true}
		if err := db.Create(&record).Error; err != nil {
			t.Fatal(err)
		}
		for _, row := range []any{&model.ClientInbound{ClientId: record.Id, InboundId: inbound.Id}, &model.ResellerClient{ResellerId: reseller.Id, Email: c.Email}} {
			if err := db.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, row := range []any{
		&model.ClientGlobalTraffic{MasterGuid: "master", Email: clients[0].Email, Up: 40, Down: 60},
		&xray.ClientTraffic{Email: clients[1].Email, InboundId: inbound.Id, Up: math.MaxInt64 - 5, Down: 10},
		&xray.ClientTraffic{Email: clients[2].Email, InboundId: inbound.Id, Up: 40, Down: 59},
	} {
		if err := db.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/panel/api/clients/delDepleted", nil))
	var body resellerWriteContract
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Success {
		t.Fatalf("delete response: %s %v", w.Body.String(), err)
	}
	for _, tc := range []struct {
		email     string
		remaining int64
	}{{clients[0].Email, 0}, {clients[1].Email, 0}, {clients[2].Email, 1}} {
		var count int64
		if err := db.Model(&model.ClientRecord{}).Where("email = ?", tc.email).Count(&count).Error; err != nil || count != tc.remaining {
			t.Fatalf("%s count %d: %v", tc.email, count, err)
		}
	}
	var archive model.ResellerTrafficArchive
	if err := db.Where("reseller_id = ? AND email = ?", reseller.Id, clients[0].Email).First(&archive).Error; err != nil || archive.ConsumedBytes != 100 {
		t.Fatalf("global usage not archived: %+v %v", archive, err)
	}
	stat, err := (&service.ResellerService{}).Stat(&reseller)
	if err != nil || stat.UsedTraffic != math.MaxInt64 {
		t.Fatalf("depleted usage lost from reseller: %+v %v", stat, err)
	}
}
