package controller

import (
	"net/http"

	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/session"

	"github.com/gin-gonic/gin"
)

// DashboardController keeps presentation choices separate from global panel
// settings. A user's layout never changes another administrator's dashboard.
type DashboardController struct {
	preferences service.DashboardPreferenceService
}

func NewDashboardController(g *gin.RouterGroup) *DashboardController {
	a := &DashboardController{}
	// Keep full literals here: API documentation tests intentionally inspect
	// route declarations and cannot infer a local RouterGroup prefix.
	g.GET("/dashboard/preference", a.getPreference)
	g.POST("/dashboard/preference", a.savePreference)
	return a
}

func (a *DashboardController) getPreference(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	view, err := a.preferences.Get(user.Id)
	jsonObj(c, view, err)
}

func (a *DashboardController) savePreference(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil || c.GetBool("api_authed") {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	var input service.DashboardPreferenceInput
	if err := c.ShouldBindJSON(&input); err != nil {
		jsonMsg(c, "invalid dashboard preference", err)
		return
	}
	view, err := a.preferences.Save(user.Id, input)
	jsonObj(c, view, err)
}
