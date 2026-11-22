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
	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func TestTrafficPeerProvisioningRequiresSuperadminSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	var admin model.User
	if err := db.First(&admin).Error; err != nil {
		t.Fatal(err)
	}
	token, err := (&panel.ApiTokenService{}).Create("provision-test")
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.Use(sessions.Sessions("phoenix", cookie.NewStore([]byte("traffic-provision-test"))))
	r.Use(func(c *gin.Context) {
		if c.GetHeader("X-Test-Admin") == "1" {
			s := sessions.Default(c)
			s.Set("LOGIN_USER", admin.Id)
			s.Set("LOGIN_EPOCH", admin.LoginEpoch)
		}
		c.Next()
	})
	api := r.Group("/panel/api")
	api.Use((&APIController{}).checkAPIAuth)
	NewNodeController(api.Group("/nodes"))
	payload, err := json.Marshal(map[string]any{"tokenId": token.Id, "sender": "master", "generation": "generation", "reconciled": true})
	if err != nil {
		t.Fatal(err)
	}
	call := func(asAdmin, asToken bool) (int, bool) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/nodes/trafficSnapshot/receiver", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		if asAdmin {
			req.Header.Set("X-Test-Admin", "1")
		}
		if asToken {
			req.Header.Set("Authorization", "Bearer "+token.Token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var response struct {
			Success bool `json:"success"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &response)
		return w.Code, response.Success
	}
	if code, success := call(false, true); code != http.StatusForbidden || success {
		t.Fatalf("API token provisioned peer: %d %v", code, success)
	}
	if _, success := call(false, false); success {
		t.Fatal("anonymous provisioned peer")
	}
	if code, success := call(true, false); code != http.StatusOK || !success {
		t.Fatalf("superadmin could not provision: %d %v", code, success)
	}
	if _, success := call(true, false); success {
		t.Fatal("duplicate registration reset state")
	}
	var count int64
	if err := db.Model(&model.TrafficSnapshotReceiver{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("registrations: %d %v", count, err)
	}
	state := model.TrafficSnapshotSender{ID: "node:99", Sender: "panel-guid", Generation: "epoch", LastSequence: 4}
	if err := db.Create(&state).Error; err != nil {
		t.Fatal(err)
	}
	read := func(withToken bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/panel/api/nodes/trafficSnapshot/sender/99", nil)
		if withToken {
			req.Header.Set("Authorization", "Bearer "+token.Token)
		} else {
			req.Header.Set("X-Test-Admin", "1")
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := read(true); w.Code != http.StatusForbidden {
		t.Fatalf("token read peer state: %d", w.Code)
	}
	w := read(false)
	var got struct {
		Success bool `json:"success"`
		Obj     struct {
			Sender       string `json:"sender"`
			Generation   string `json:"generation"`
			LastSequence int64  `json:"lastSequence"`
		} `json:"obj"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !got.Success || got.Obj.Sender != state.Sender || got.Obj.Generation != state.Generation || got.Obj.LastSequence != 4 {
		t.Fatalf("sender read: %s %v", fmt.Sprint(w.Body), err)
	}

}
