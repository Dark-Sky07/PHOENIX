package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestVerifiedGlobalEndpointBindsBearerTokenAndRegisteredSender(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	tokens := &panel.ApiTokenService{}
	one, err := tokens.Create("master-one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := tokens.Create("master-two")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []*panel.ApiTokenView{one, two} {
		if err := db.Create(&model.TrafficSnapshotReceiver{ID: fmt.Sprintf("api-token:%d", token.Id), Sender: map[int]string{one.Id: "sender-one", two.Id: "sender-two"}[token.Id], Generation: "g"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&xray.ClientTraffic{Email: "verified@example.test", Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(sessions.Sessions("phoenix", cookie.NewStore([]byte("traffic-test-secret"))))
	group := r.Group("/panel/api")
	group.Use((&APIController{}).checkAPIAuth)
	NewInboundController(group.Group("/inbounds"))
	guid, err := (&service.SettingService{}).GetPanelGuid()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.TrafficSnapshotSender{ID: fmt.Sprintf("node-snapshot:api-token:%d", one.Id), Sender: guid, Generation: "node-epoch"}).Error; err != nil {
		t.Fatal(err)
	}
	fetchNodeSnapshot := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/panel/api/inbounds/verifiedNodeSnapshot", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	var produced struct {
		Success bool                     `json:"success"`
		Obj     trafficprotocol.Envelope `json:"obj"`
	}
	wNode := fetchNodeSnapshot(one.Token)
	if err := json.Unmarshal(wNode.Body.Bytes(), &produced); err != nil || !produced.Success || wNode.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("node snapshot: %d %s %v", wNode.Code, wNode.Body.String(), err)
	}
	nodeKey, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(one.Token)
	if err != nil {
		t.Fatal(err)
	}
	if err := trafficprotocol.Verify(produced.Obj, nodeKey, trafficprotocol.Expected{Sender: guid, Generation: "node-epoch"}); err != nil {
		t.Fatal(err)
	}
	otherNode := fetchNodeSnapshot(two.Token)
	var rejected struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal(otherNode.Body.Bytes(), &rejected)
	if rejected.Success {
		t.Fatal("unprovisioned token obtained snapshot")
	}
	payload, err := json.Marshal([]*xray.ClientTraffic{{Email: "verified@example.test", Up: 123}})
	if err != nil {
		t.Fatal(err)
	}
	key, err := trafficprotocol.KeyFromAPIToken(one.Token)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := trafficprotocol.NewReadinessChallenge("sender-one", "g", key)
	if err != nil {
		t.Fatal(err)
	}
	challengeBody, err := json.Marshal(challenge)
	if err != nil {
		t.Fatal(err)
	}
	readiness := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/panel/api/inbounds/trafficReadiness", bytes.NewReader(challengeBody))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := readiness(two.Token); w.Code != http.StatusForbidden {
		t.Fatalf("wrong peer status %d", w.Code)
	}
	w := readiness(one.Token)
	var ready struct {
		Success bool                              `json:"success"`
		Obj     trafficprotocol.ReadinessResponse `json:"obj"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ready); err != nil || w.Code != http.StatusOK || !ready.Success {
		t.Fatalf("readiness failed: %d %s %v", w.Code, w.Body.String(), err)
	}
	if err := trafficprotocol.VerifyReadiness(challenge, ready.Obj, key, 0); err != nil {
		t.Fatal(err)
	}
	var before model.TrafficSnapshotReceiver
	if err := db.First(&before, "id = ?", fmt.Sprintf("api-token:%d", one.Id)).Error; err != nil || before.LastSequence != 0 {
		t.Fatalf("readiness mutated state: %+v %v", before, err)
	}
	e, err := trafficprotocol.Sign(trafficprotocol.Envelope{Version: trafficprotocol.Version, Sender: "sender-one", Generation: "g", Sequence: 1, Snapshot: payload}, key)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	call := func(token string) (int, bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/inbounds/pushVerifiedClientTraffics", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var response struct {
			Success bool `json:"success"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		return w.Code, response.Success
	}
	if _, success := call(two.Token); success {
		t.Fatal("other token accepted signed snapshot")
	}
	if code, success := call(one.Token); code != http.StatusOK || !success {
		t.Fatalf("registered token rejected: status %d success %v", code, success)
	}
	if _, success := call(one.Token); success {
		t.Fatal("replay accepted")
	}
	var row model.ClientGlobalTraffic
	if err := db.Where("master_guid = ? AND email = ?", "sender-one", "verified@example.test").First(&row).Error; err != nil || row.Up != 123 {
		t.Fatalf("traffic: %+v %v", row, err)
	}
	var other model.TrafficSnapshotReceiver
	if err := db.First(&other, "id = ?", fmt.Sprintf("api-token:%d", two.Id)).Error; err != nil || other.LastSequence != 0 {
		t.Fatalf("other token sequence changed: %+v %v", other, err)
	}
}
