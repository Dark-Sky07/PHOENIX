package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/middleware"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/Dark-Sky07/phoenix/internal/web/session"

	"github.com/gin-gonic/gin"
)

type NodeController struct {
	nodeService service.NodeService
}

func NewNodeController(g *gin.RouterGroup) *NodeController {
	a := &NodeController{}
	a.initRouter(g)
	return a
}

func (a *NodeController) initRouter(g *gin.RouterGroup) {
	g.GET("/list", a.list)
	g.GET("/get/:id", a.get)
	g.GET("/webCert/:id", a.webCert)

	g.POST("/add", a.add)
	g.POST("/update/:id", a.update)
	g.POST("/del/:id", a.del)
	g.POST("/setEnable/:id", a.setEnable)

	g.POST("/test", a.test)
	g.POST("/certFingerprint", a.certFingerprint)
	g.POST("/inbounds", a.inbounds)
	g.POST("/probe/:id", a.probe)
	g.POST("/trafficSnapshot/receiver", a.registerTrafficReceiver)
	g.POST("/nodeSnapshot/producer", a.registerNodeSnapshotProducer)
	g.GET("/nodeSnapshot/producer/:tokenId", a.getNodeSnapshotProducer)
	g.POST("/nodeSnapshot/receiver/:id", a.registerNodeSnapshotReceiver)
	g.POST("/trafficSnapshot/sender/:id", a.registerTrafficSender)
	g.POST("/trafficSnapshot/sender/:id/activate", a.activateTrafficSender)
	g.GET("/trafficSnapshot/sender/:id", a.getTrafficSender)
	g.POST("/updatePanel", a.updatePanel)
	g.GET("/history/:id/:metric/:bucket", a.history)
}

// Provisioning changes accounting trust and must require an interactive
// superadmin session, never an API token or reseller session.
func canProvisionTrafficPeers(c *gin.Context) bool {
	if c.GetBool("api_authed") || !panel.IsSuperAdmin(session.GetLoginUser(c)) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"success": false, "msg": "superadmin session required"})
		return false
	}
	return true
}

func (a *NodeController) registerNodeSnapshotProducer(c *gin.Context) {
	if !canProvisionTrafficPeers(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var req struct {
		TokenID    int  `json:"tokenId"`
		Reconciled bool `json:"reconciled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonMsg(c, "invalid node snapshot producer", err)
		return
	}
	state, err := service.RegisterNodeSnapshotProducer(req.TokenID, req.Reconciled)
	if err != nil {
		jsonMsg(c, "node snapshot producer registration failed", err)
		return
	}
	jsonObj(c, gin.H{"sender": state.Sender, "generation": state.Generation, "lastSequence": state.LastSequence}, nil)
}

func (a *NodeController) getNodeSnapshotProducer(c *gin.Context) {
	if !canProvisionTrafficPeers(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("tokenId"))
	if err != nil || id <= 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	var state model.TrafficSnapshotSender
	if err := database.GetDB().First(&state, "id = ?", fmt.Sprintf("node-snapshot:api-token:%d", id)).Error; err != nil {
		jsonMsg(c, "node snapshot producer not available", err)
		return
	}
	jsonObj(c, gin.H{"sender": state.Sender, "generation": state.Generation, "lastSequence": state.LastSequence}, nil)
}

func (a *NodeController) registerNodeSnapshotReceiver(c *gin.Context) {
	if !canProvisionTrafficPeers(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var req struct {
		Sender     string `json:"sender"`
		Generation string `json:"generation"`
		Reconciled bool   `json:"reconciled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonMsg(c, "invalid node snapshot receiver", err)
		return
	}
	if err := service.RegisterNodeSnapshotReceiver(c.Request.Context(), id, req.Sender, req.Generation, req.Reconciled); err != nil {
		jsonMsg(c, "node snapshot receiver registration failed", err)
		return
	}
	jsonMsg(c, "node snapshot receiver registered; legacy sync disabled", nil)
}

func (a *NodeController) registerTrafficReceiver(c *gin.Context) {
	if !canProvisionTrafficPeers(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var req struct {
		TokenID    int    `json:"tokenId"`
		Sender     string `json:"sender"`
		Generation string `json:"generation"`
		Reconciled bool   `json:"reconciled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonMsg(c, "invalid verified traffic receiver", err)
		return
	}
	if err := service.RegisterVerifiedTrafficReceiver(req.TokenID, req.Sender, req.Generation, req.Reconciled); err != nil {
		jsonMsg(c, "verified traffic receiver registration failed", err)
		return
	}
	jsonMsg(c, "verified traffic receiver registered", nil)
}

func (a *NodeController) registerTrafficSender(c *gin.Context) {
	if !canProvisionTrafficPeers(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "invalid node", err)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var req struct {
		Reconciled bool `json:"reconciled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonMsg(c, "invalid verified traffic sender", err)
		return
	}
	state, err := service.RegisterVerifiedTrafficSender(id, req.Reconciled)
	if err != nil {
		jsonMsg(c, "verified traffic sender registration failed", err)
		return
	}
	jsonObj(c, gin.H{"sender": state.Sender, "generation": state.Generation, "lastSequence": state.LastSequence}, nil)
}

func (a *NodeController) activateTrafficSender(c *gin.Context) {
	if !canProvisionTrafficPeers(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var req struct {
		Reconciled bool `json:"reconciled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonMsg(c, "invalid sender activation", err)
		return
	}
	if err := service.ActivateVerifiedTrafficSender(c.Request.Context(), id, req.Reconciled); err != nil {
		jsonMsg(c, "verified traffic activation failed", err)
		return
	}
	jsonMsg(c, "verified traffic sender activated", nil)
}

func (a *NodeController) getTrafficSender(c *gin.Context) {
	if !canProvisionTrafficPeers(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	var state model.TrafficSnapshotSender
	if err := database.GetDB().First(&state, "id = ?", fmt.Sprintf("node:%d", id)).Error; err != nil {
		jsonMsg(c, "verified traffic sender not available", err)
		return
	}
	jsonObj(c, gin.H{"sender": state.Sender, "generation": state.Generation, "lastSequence": state.LastSequence}, nil)
}

func (a *NodeController) list(c *gin.Context) {
	nodes, err := a.nodeService.GetNodeTree()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.list"), err)
		return
	}
	jsonObj(c, nodes, nil)
}

func (a *NodeController) get(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	n, err := a.nodeService.GetById(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.obtain"), err)
		return
	}
	jsonObj(c, n, nil)
}

// webCert returns the node's own web TLS certificate/key file paths so the
// inbound form's "Set Cert from Panel" can fill paths that exist on the node.
func (a *NodeController) webCert(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	files, err := a.nodeService.GetWebCertFiles(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.obtain"), err)
		return
	}
	jsonObj(c, files, nil)
}

func (a *NodeController) ensureReachable(c *gin.Context, n *model.Node) error {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
	defer cancel()
	if _, err := a.nodeService.Probe(ctx, n); err != nil {
		return errors.New(service.FriendlyProbeError(err.Error()))
	}
	return nil
}

func (a *NodeController) add(c *gin.Context) {
	n, ok := middleware.BindAndValidate[model.Node](c)
	if !ok {
		return
	}
	if err := a.ensureReachable(c, n); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.add"), err)
		return
	}
	if err := a.nodeService.Create(n); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.add"), err)
		return
	}
	jsonMsgObj(c, I18nWeb(c, "pages.nodes.toasts.add"), n, nil)
}

func (a *NodeController) update(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	n, ok := middleware.BindAndValidate[model.Node](c)
	if !ok {
		return
	}
	if err := a.ensureReachable(c, n); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.update"), err)
		return
	}
	if err := a.nodeService.Update(id, n); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.update"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.update"), nil)
}

func (a *NodeController) del(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	if err := a.nodeService.Delete(id); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.delete"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.delete"), nil)
}

func (a *NodeController) setEnable(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	body := struct {
		Enable bool `json:"enable" form:"enable"`
	}{}
	if err := c.ShouldBind(&body); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.update"), err)
		return
	}
	if err := a.nodeService.SetEnable(id, body.Enable); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.update"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.update"), nil)
}

func (a *NodeController) inbounds(c *gin.Context) {
	n := &model.Node{}
	if err := c.ShouldBind(n); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.obtain"), err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	options, err := a.nodeService.GetRemoteInboundOptions(ctx, n)
	jsonObj(c, options, err)
}

func (a *NodeController) test(c *gin.Context) {
	n := &model.Node{}
	if err := c.ShouldBind(n); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.test"), err)
		return
	}
	if n.Scheme == "" {
		n.Scheme = "https"
	}
	if n.BasePath == "" {
		n.BasePath = "/"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
	defer cancel()
	patch, err := a.nodeService.Probe(ctx, n)
	jsonObj(c, patch.ToUI(err == nil), nil)
}

func (a *NodeController) certFingerprint(c *gin.Context) {
	n := &model.Node{}
	if err := c.ShouldBind(n); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.test"), err)
		return
	}
	if n.Scheme == "" {
		n.Scheme = "https"
	}
	if n.BasePath == "" {
		n.BasePath = "/"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
	defer cancel()
	fp, err := a.nodeService.FetchCertFingerprint(ctx, n)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.test"), err)
		return
	}
	jsonObj(c, fp, nil)
}

func (a *NodeController) probe(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	n, err := a.nodeService.GetById(id)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.nodes.toasts.obtain"), err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 6*time.Second)
	defer cancel()
	patch, probeErr := a.nodeService.Probe(ctx, n)
	if probeErr != nil {
		patch.Status = "offline"
	} else {
		patch.Status = "online"
	}
	_ = a.nodeService.UpdateHeartbeat(id, patch)
	jsonObj(c, patch.ToUI(probeErr == nil), nil)
}

func (a *NodeController) updatePanel(c *gin.Context) {
	var req struct {
		Ids []int `json:"ids"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), err)
		return
	}
	if len(req.Ids) == 0 {
		jsonMsg(c, I18nWeb(c, "somethingWentWrong"), fmt.Errorf("no nodes selected"))
		return
	}
	results, err := a.nodeService.UpdatePanels(req.Ids)
	jsonMsgObj(c, I18nWeb(c, "pages.nodes.toasts.updateStarted"), results, err)
}

func (a *NodeController) history(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "get"), err)
		return
	}
	metric := c.Param("metric")
	if !slices.Contains(service.NodeMetricKeys, metric) {
		jsonMsg(c, "invalid metric", fmt.Errorf("unknown metric"))
		return
	}
	bucket, err := strconv.Atoi(c.Param("bucket"))
	if err != nil || bucket <= 0 || !service.IsAllowedHistoryBucket(bucket) {
		jsonMsg(c, "invalid bucket", fmt.Errorf("unsupported bucket"))
		return
	}
	jsonObj(c, a.nodeService.AggregateNodeMetric(id, metric, bucket, 60), nil)
}
