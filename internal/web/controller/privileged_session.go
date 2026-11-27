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
