package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/session"

	"github.com/gin-gonic/gin"
)

func bearerSuperadminContext(path string) (*gin.Context, *httptest.ResponseRecorder) {
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, path, nil)
	// API authentication deliberately supplies the primary user for ordinary
	// full-scope API compatibility. Privileged handlers must distinguish this
	// synthetic principal from an interactive login.
	ctx.Set("api_authed", true)
	session.SetAPIAuthUser(ctx, &model.User{Id: 1})
	return ctx, writer
}

func TestHighImpactHandlersRejectBearerTokenPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := &SettingController{}
	server := &ServerController{}
	cases := []struct {
		name    string
		path    string
		handler func(*gin.Context)
	}{
		{"telegram backup", "/panel/api/backuptotgbot", (&APIController{}).BackuptoTgbot},
		{"list API tokens", "/panel/api/setting/apiTokens", settings.listApiTokens},
		{"create API token", "/panel/api/setting/apiTokens/create", settings.createApiToken},
		{"delete API token", "/panel/api/setting/apiTokens/delete/1", settings.deleteApiToken},
		{"enable API token", "/panel/api/setting/apiTokens/setEnabled/1", settings.setApiTokenEnabled},
		{"list admins", "/panel/api/setting/admins", settings.listAdmins},
		{"create admin", "/panel/api/setting/admins/create", settings.createAdmin},
		{"update admin", "/panel/api/setting/admins/update/2", settings.updateAdmin},
		{"delete admin", "/panel/api/setting/admins/delete/2", settings.deleteAdmin},
		{"issue certificate", "/panel/api/setting/cert/issue", settings.issueCert},
		{"cloud backup", "/panel/api/setting/cloudBackup/run", settings.runCloudBackup},
		{"clear audit logs", "/panel/api/setting/auditLogs/clear", settings.clearAuditLogs},
		{"database download", "/panel/api/server/getDb", server.getDb},
		{"database migration download", "/panel/api/server/getMigration", server.getMigration},
		{"database restore", "/panel/api/server/importDB", server.importDB},
		{"accounting reconciliation repair", "/panel/api/resellers/reconciliation/repair", (&ResellerController{}).reconciliationRepair},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, writer := bearerSuperadminContext(tc.path)
			tc.handler(ctx)
			if writer.Code != http.StatusForbidden {
				t.Fatalf("bearer token principal reached %s: status=%d body=%s", tc.name, writer.Code, writer.Body.String())
			}
		})
	}
}

func TestInteractiveSuperadminStillPassesPrivilegedGate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/panel/api/setting/apiTokens/create", nil)
	session.SetAPIAuthUser(ctx, &model.User{Id: 1})

	if !requireInteractiveSuperAdmin(ctx) {
		t.Fatal("interactive primary-user session was rejected by privileged gate")
	}
}
