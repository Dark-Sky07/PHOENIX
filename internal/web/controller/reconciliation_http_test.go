package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/web/middleware"
	"github.com/Dark-Sky07/phoenix/internal/web/session"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

// TestReconciliationRepairBrowserContract exercises the complete API middleware
// sequence used by the browser: authenticated session, CSRF validation, RBAC,
// interactive-superadmin gate, and the repair handler. The former SPA default
// URL-encoded this request while the handler required JSON, causing its HTTP
// 400. JSON is now explicit in the SPA and the legacy form is accepted so a
// cached browser cannot strand recovery behind that mismatch.
func TestReconciliationRepairBrowserContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })

	admin := seedSecurityTestAdmin(t)

	router := gin.New()
	router.Use(sessions.Sessions("phoenix", cookie.NewStore([]byte("reconciliation-http-contract-test-secret"))))
	router.GET("/test/login", func(c *gin.Context) {
		if err := session.SetLoginUser(c, &admin); err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.Status(http.StatusNoContent)
	})
	router.GET("/csrf-token", func(c *gin.Context) {
		token, err := session.EnsureCSRFToken(c)
		if err != nil {
			c.String(http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"success": true, "obj": token})
	})

	api := router.Group("/panel/api")
	apiController := &APIController{}
	api.Use(apiController.checkAPIAuth)
	api.Use(middleware.CSRFMiddleware())
	api.Use(middleware.ResellerGuard())
	api.Use(middleware.AdminRBACGuard())
	api.Use(apiController.auditMiddleware)
	NewResellerController(api.Group("/resellers"))

	login := httptest.NewRecorder()
	router.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/test/login", nil))
	if login.Code != http.StatusNoContent {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	loginCookie := reconciliationCookie(t, login)

	csrfReq := httptest.NewRequest(http.MethodGet, "/csrf-token", nil)
	csrfReq.AddCookie(loginCookie)
	csrfRec := httptest.NewRecorder()
	router.ServeHTTP(csrfRec, csrfReq)
	if csrfRec.Code != http.StatusOK {
		t.Fatalf("csrf bootstrap status=%d body=%s", csrfRec.Code, csrfRec.Body.String())
	}
	var csrfResponse struct {
		Success bool   `json:"success"`
		Token   string `json:"obj"`
	}
	if err := json.Unmarshal(csrfRec.Body.Bytes(), &csrfResponse); err != nil || !csrfResponse.Success || csrfResponse.Token == "" {
		t.Fatalf("csrf bootstrap response=%s err=%v", csrfRec.Body.String(), err)
	}
	sessionCookie := reconciliationCookie(t, csrfRec)

	call := func(contentType, body string, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/resellers/reconciliation/repair", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", contentType)
		if csrf {
			req.Header.Set(session.CSRFHeaderName, csrfResponse.Token)
		}
		req.AddCookie(sessionCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
	}{
		{"current JSON SPA", "application/json", `{"confirm":"","dryRun":true}`},
		{"cached form SPA", "application/x-www-form-urlencoded", "confirm=&dryRun=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := call(tc.contentType, tc.body, true)
			if rec.Code != http.StatusOK {
				t.Fatalf("repair status=%d body=%s", rec.Code, rec.Body.String())
			}
			var response struct {
				Success bool `json:"success"`
				Obj     struct {
					DryRun bool `json:"dryRun"`
				} `json:"obj"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || !response.Success || !response.Obj.DryRun {
				t.Fatalf("repair response=%s err=%v", rec.Body.String(), err)
			}
		})
	}

	if rec := call("application/json", `{"confirm":"","dryRun":true}`, false); rec.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s, want 403", rec.Code, rec.Body.String())
	}
	if rec := call("application/json", `{`, true); rec.Code != http.StatusBadRequest || !bytes.Contains(rec.Body.Bytes(), []byte("invalid reconciliation request")) {
		t.Fatalf("malformed JSON status=%d body=%s, want explained 400", rec.Code, rec.Body.String())
	}
}

func reconciliationCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	cookies := rec.Result().Cookies()
	for i := len(cookies) - 1; i >= 0; i-- {
		if cookies[i].Name == "phoenix" {
			return cookies[i]
		}
	}
	t.Fatalf("phoenix session cookie missing from response: %v", rec.Header().Values("Set-Cookie"))
	return nil
}
