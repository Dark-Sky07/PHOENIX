package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/middleware"
	"github.com/gin-gonic/gin"
)

// A session revoked after authentication must not be reinterpreted as an
// administrator by the reseller guard or scoped handlers.
func TestRevocationDuringRequestDoesNotEscalateReseller(t *testing.T) {
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/panel/api/server/probe", http.StatusForbidden},
		{"/panel/api/clients/principal-probe", http.StatusNoContent},
	} {
		t.Run(tc.path, func(t *testing.T) {
			router := setupResellerInboundRouter(t)
			api := router.Group("/panel/api")
			api.Use((&APIController{}).checkAPIAuth)
			api.Use(func(c *gin.Context) {
				if err := database.GetDB().Model(&model.Reseller{}).Where("username = ?", "sara").Update("login_epoch", 2).Error; err != nil {
					c.AbortWithStatus(http.StatusInternalServerError)
					return
				}
				c.Next()
			})
			api.Use(middleware.ResellerGuard())
			api.GET("/server/probe", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			api.GET("/clients/principal-probe", func(c *gin.Context) {
				if resellerSession(c) == nil {
					c.Status(http.StatusInternalServerError)
					return
				}
				c.Status(http.StatusNoContent)
			})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
