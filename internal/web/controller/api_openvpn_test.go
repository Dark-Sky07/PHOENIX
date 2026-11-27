package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/session"
	"github.com/gin-gonic/gin"
)

func TestRevokeOpenVPNClientRejectsBearerTokenPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	writer := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/panel/api/openvpn/revoke", bytes.NewBufferString(`{"email":"client@example.test"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	// checkAPIAuth gives bearer callers the primary user for regular API
	// compatibility. The endpoint must still distinguish that synthetic
	// principal from a superadmin browser session.
	ctx.Set("api_authed", true)
	session.SetAPIAuthUser(ctx, &model.User{Id: 1})

	(&APIController{}).RevokeOpenVPNClient(ctx)
	if writer.Code != http.StatusForbidden {
		t.Fatalf("bearer token principal reached OpenVPN revocation: status=%d body=%s", writer.Code, writer.Body.String())
	}
}
