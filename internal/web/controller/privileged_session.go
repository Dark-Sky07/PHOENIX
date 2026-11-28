package controller

import (
	"net/http"

	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/Dark-Sky07/phoenix/internal/web/session"

	"github.com/gin-gonic/gin"
)

// requireInteractiveSuperAdmin reserves high-impact operations for a real
// superadmin browser session. checkAPIAuth deliberately maps a full-scope API
// token to the primary user for ordinary documented API automation, so checking
// only the resulting user is insufficient here: a copied token must not gain
// authority to create admins/tokens, erase evidence, export or restore a full
// database, or trigger an external backup.
func requireInteractiveSuperAdmin(c *gin.Context) bool {
	if c.GetBool("api_authed") || !panel.IsSuperAdmin(session.GetLoginUser(c)) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
			"success": false,
			"msg":     "superadmin browser session required for this operation",
		})
		return false
	}
	return true
}

// requireRecentInteractiveSuperAdmin adds a short proof-of-presence check to
// operations that mint credentials or alter another administrator's ability to
// access the panel. A successful login or /reauth grants the proof for a
// limited interval; a bearer token is never an alternative credential.
func requireRecentInteractiveSuperAdmin(c *gin.Context) bool {
	if !requireInteractiveSuperAdmin(c) {
		return false
	}
	if session.HasRecentAuthentication(c) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"success": false,
		"msg":     "recent reauthentication required; confirm your password before this sensitive action",
	})
	return false
}
