package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/gin-gonic/gin"
)

// FleetController is the operator-facing Phase C control plane. These routes
// use the normal panel authentication chain and deliberately require recent
// interactive superadmin authentication for every credential/config mutation.
type FleetController struct {
	fleet service.FleetService
}

func NewFleetController(g *gin.RouterGroup) *FleetController {
	a := &FleetController{}
	a.initRouter(g)
	return a
}

func (a *FleetController) initRouter(g *gin.RouterGroup) {
	g.GET("/templates", a.listTemplates)
	g.GET("/templates/:id", a.getTemplate)
	g.POST("/templates", a.createTemplate)
	g.POST("/templates/:id/disable", a.setTemplateEnabled)
	g.POST("/templates/:id/revisions", a.createRevision)
	g.POST("/templates/:id/deploy", a.deploy)
	g.POST("/templates/:id/rollback", a.rollback)

	g.GET("/pools", a.listPools)
	g.POST("/pools", a.createPool)
	g.POST("/pools/:id", a.updatePool)
	g.POST("/pools/:id/delete", a.deletePool)

	g.GET("/enrollments", a.listEnrollments)
	g.POST("/enrollments", a.createEnrollment)
	g.POST("/enrollments/:id/installer", a.createEnrollmentInstaller)
	g.POST("/enrollments/:id/update", a.requestEnrollmentUpdate)
	g.POST("/enrollments/:id/rotate", a.rotateEnrollment)
	g.POST("/enrollments/:id/disable", a.setEnrollmentEnabled)

	g.GET("/tunnels", a.listTunnels)
	g.POST("/tunnels", a.createTunnel)
	g.POST("/tunnels/:id", a.updateTunnel)
	g.POST("/tunnels/:id/delete", a.deleteTunnel)
}

func (a *FleetController) listTemplates(c *gin.Context) {
	rows, err := a.fleet.ListTemplates()
	jsonObj(c, rows, err)
}

func (a *FleetController) getTemplate(c *gin.Context) {
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet template id", err)
		return
	}
	row, err := a.fleet.GetTemplate(id)
	jsonObj(c, row, err)
}

func (a *FleetController) createTemplate(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	var in service.FleetTemplateInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet template", err)
		return
	}
	row, err := a.fleet.CreateTemplate(in)
	jsonObj(c, row, err)
}

func (a *FleetController) setTemplateEnabled(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet template id", err)
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet template state", err)
		return
	}
	row, err := a.fleet.SetTemplateEnabled(id, in.Enabled)
	jsonObj(c, row, err)
}

func (a *FleetController) createRevision(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet template id", err)
		return
	}
	var in service.FleetTemplateRevisionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet template revision", err)
		return
	}
	row, err := a.fleet.CreateRevision(id, in)
	jsonObj(c, row, err)
}

func (a *FleetController) deploy(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet template id", err)
		return
	}
	var in service.FleetDeploymentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet deployment", err)
		return
	}
	rows, err := a.fleet.Deploy(id, in)
	jsonObj(c, rows, err)
}

// Rollback is intentionally the same explicit-target machinery as deploy;
// callers select an already immutable prior revision and the canary cohort to
// return to it. Naming it separately keeps the operator API self-documenting.
func (a *FleetController) rollback(c *gin.Context) {
	a.deploy(c)
}

func (a *FleetController) listPools(c *gin.Context) {
	rows, err := a.fleet.ListPools()
	jsonObj(c, rows, err)
}

func (a *FleetController) createPool(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	var in service.FleetPoolInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet pool", err)
		return
	}
	row, err := a.fleet.CreatePool(in)
	jsonObj(c, row, err)
}

func (a *FleetController) updatePool(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet pool id", err)
		return
	}
	var in service.FleetPoolInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet pool", err)
		return
	}
	row, err := a.fleet.UpdatePool(id, in)
	jsonObj(c, row, err)
}

func (a *FleetController) deletePool(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err == nil {
		err = a.fleet.DeletePool(id)
	}
	jsonMsg(c, "fleet pool deleted", err)
}

func (a *FleetController) listEnrollments(c *gin.Context) {
	rows, err := a.fleet.ListEnrollments()
	jsonObj(c, rows, err)
}

func (a *FleetController) createEnrollment(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	var in service.FleetEnrollmentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet enrollment", err)
		return
	}
	row, err := a.fleet.CreateEnrollment(in)
	jsonObj(c, row, err)
}

// createEnrollmentInstaller creates a reviewed, one-time local bootstrap
// script. It intentionally returns text for the operator to run on the node;
// it never makes the panel dial a node or carry SSH credentials.
func (a *FleetController) createEnrollmentInstaller(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet enrollment id", err)
		return
	}
	var in service.FleetInstallerInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet installer", err)
		return
	}
	row, err := a.fleet.CreateEnrollmentInstaller(id, in)
	jsonObj(c, row, err)
}

func (a *FleetController) requestEnrollmentUpdate(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet enrollment id", err)
		return
	}
	row, err := a.fleet.RequestEnrollmentUpdate(id)
	jsonObj(c, row, err)
}

func (a *FleetController) rotateEnrollment(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet enrollment id", err)
		return
	}
	row, err := a.fleet.RotateEnrollmentCredential(id)
	jsonObj(c, row, err)
}

func (a *FleetController) setEnrollmentEnabled(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid fleet enrollment id", err)
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid fleet enrollment state", err)
		return
	}
	row, err := a.fleet.SetEnrollmentEnabled(id, in.Enabled)
	jsonObj(c, row, err)
}

func (a *FleetController) listTunnels(c *gin.Context) {
	rows, err := a.fleet.ListTunnels()
	jsonObj(c, rows, err)
}

func (a *FleetController) createTunnel(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	var in service.ManagedTunnelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid managed tunnel", err)
		return
	}
	row, err := a.fleet.CreateTunnel(in)
	jsonObj(c, row, err)
}

func (a *FleetController) updateTunnel(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err != nil {
		jsonMsg(c, "invalid managed tunnel id", err)
		return
	}
	var in service.ManagedTunnelInput
	if err := c.ShouldBindJSON(&in); err != nil {
		jsonMsg(c, "invalid managed tunnel", err)
		return
	}
	row, err := a.fleet.UpdateTunnel(id, in)
	jsonObj(c, row, err)
}

func (a *FleetController) deleteTunnel(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := fleetPathID(c)
	if err == nil {
		err = a.fleet.DeleteTunnel(id)
	}
	jsonMsg(c, "managed tunnel deleted", err)
}

func fleetPathID(c *gin.Context) (int, error) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		return 0, errors.New("id must be positive")
	}
	return id, nil
}

// FleetAgentController intentionally lives outside the browser /panel/api
// middleware chain: Node Agents have their own narrow, outbound-only bearer
// protocol and must not be redirected to login or asked for a CSRF cookie.
type FleetAgentController struct {
	fleet service.FleetService
}

func NewFleetAgentController(g *gin.RouterGroup) *FleetAgentController {
	a := &FleetAgentController{}
	agent := g.Group("/panel/agent/v1")
	agent.POST("/enroll", a.enroll)
	agent.POST("/checkin", a.checkIn)
	return a
}

func (a *FleetAgentController) enroll(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	var in service.AgentEnrollRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		fleetAgentError(c, http.StatusBadRequest, "invalid enrollment request")
		return
	}
	out, err := a.fleet.AgentEnroll(c.Request.Context(), in)
	if err != nil {
		fleetAgentServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func (a *FleetAgentController) checkIn(c *gin.Context) {
	credential, ok := fleetAgentBearer(c.GetHeader("Authorization"))
	if !ok {
		fleetAgentError(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16*1024)
	var in service.AgentCheckInRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		fleetAgentError(c, http.StatusBadRequest, "invalid check-in request")
		return
	}
	out, err := a.fleet.AgentCheckIn(c.Request.Context(), credential, in)
	if err != nil {
		fleetAgentServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

func fleetAgentBearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || header[:len(prefix)] != prefix {
		return "", false
	}
	value := header[len(prefix):]
	if len(value) < 32 || len(value) > 256 {
		return "", false
	}
	return value, true
}

func fleetAgentServiceError(c *gin.Context, err error) {
	if errors.Is(err, service.ErrFleetAgentUnauthorized()) {
		fleetAgentError(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	fleetAgentError(c, http.StatusBadRequest, "fleet agent request rejected")
}

func fleetAgentError(c *gin.Context, status int, message string) {
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatusJSON(status, gin.H{"error": message})
}
