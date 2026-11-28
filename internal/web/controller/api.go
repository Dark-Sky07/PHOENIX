package controller

import (
	"net/http"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/openvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/middleware"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/Dark-Sky07/phoenix/internal/web/service/tgbot"
	"github.com/Dark-Sky07/phoenix/internal/web/session"

	"github.com/gin-gonic/gin"
)

// APIController handles the main API routes for the PHOENIX panel, including inbounds and server management.
type APIController struct {
	BaseController
	inboundController      *InboundController
	serverController       *ServerController
	nodeController         *NodeController
	settingController      *SettingController
	xraySettingController  *XraySettingController
	resellerController     *ResellerController
	resellerSelfController *ResellerSelfController
	authController         *AuthController
	settingService         service.SettingService
	userService            panel.UserService
	apiTokenService        panel.ApiTokenService
	auditLogService        service.AuditLogService
	Tgbot                  tgbot.Tgbot
}

// NewAPIController creates a new APIController instance and initializes its routes.
func NewAPIController(g *gin.RouterGroup) *APIController {
	a := &APIController{}
	a.initRouter(g)
	return a
}

func (a *APIController) checkAPIAuth(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	if after, ok := strings.CutPrefix(auth, "Bearer "); ok {
		tok := after
		if token, ok := a.apiTokenService.Authenticate(tok); ok {
			// Explicit granular permissions are checked for every route. Empty
			// Scopes preserves the established legacy full/read behavior.
			if !panel.TokenAllowsRequest(token, c.Request.Method, c.Request.URL.Path) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			if u, err := a.userService.GetFirstUser(); err == nil {
				session.SetAPIAuthUser(c, u)
			}
			c.Set("api_authed", true)
			c.Set("api_token_name", token.Name)
			c.Set("api_token_id", token.Id)
			c.Next()
			return
		}
	}
	user := session.GetLoginUser(c)
	var reseller *model.Reseller
	if user == nil {
		reseller = session.GetCurrentLoginReseller(c)
	}
	if user == nil && reseller == nil {
		if c.GetHeader("X-Requested-With") == "XMLHttpRequest" {
			c.AbortWithStatus(http.StatusUnauthorized)
		} else {
			c.AbortWithStatus(http.StatusNotFound)
		}
		return
	}
	session.PinLoginReseller(c, reseller)
	c.Next()
}

// initRouter sets up the API routes for inbounds, server, and other endpoints.
func (a *APIController) initRouter(g *gin.RouterGroup) {
	// Main API group
	api := g.Group("/panel/api")
	api.Use(a.checkAPIAuth)
	api.Use(middleware.CSRFMiddleware())
	// Reseller (نمایندگی) sessions are confined to their own slice of the API.
	api.Use(middleware.ResellerGuard())
	// Multi-admin RBAC enforcement for secondary admin accounts.
	api.Use(middleware.AdminRBACGuard())
	// Security audit log for mutating API operations.
	api.Use(a.auditMiddleware)

	// Inbounds API
	inbounds := api.Group("/inbounds")
	a.inboundController = NewInboundController(inbounds)

	clients := api.Group("/clients")
	NewClientController(clients)
	NewGroupController(clients)

	// Server API
	server := api.Group("/server")
	a.serverController = NewServerController(server)

	// Nodes API — multi-panel management
	nodes := api.Group("/nodes")
	a.nodeController = NewNodeController(nodes)

	// Settings + Xray config management live under the API surface too, so the
	// same API token drives them. Paths are /panel/api/setting/* and
	// /panel/api/xray/*.
	a.settingController = NewSettingController(api)
	a.xraySettingController = NewXraySettingController(api)

	// Reseller (نمایندگی) management — admin CRUD under /panel/api/resellers/*,
	// the reseller's own endpoints under /panel/api/reseller/*.
	a.resellerController = NewResellerController(api.Group("/resellers"))
	a.resellerSelfController = NewResellerSelfController(api.Group("/reseller"))
	a.authController = NewAuthController(api.Group("/auth"), a.userService)

	// Extra routes
	api.POST("/backuptotgbot", a.BackuptoTgbot)
	api.POST("/openvpn/revoke", a.RevokeOpenVPNClient)
}

// RevokeOpenVPNClient removes a client from the generated certificate
// allowlist and kills its currently managed sessions. It is an explicit
// superadmin action: the caller still has to verify final accounting before
// releasing quota or deleting the client, and the response never claims that
// an acknowledged kill was fully drained.
func (a *APIController) RevokeOpenVPNClient(c *gin.Context) {
	// checkAPIAuth maps a bearer token to the primary user so ordinary full
	// token automation can use the documented panel API. Certificate revocation
	// is deliberately narrower: it kills live sessions and needs an explicit
	// operator review. Do not let that synthetic principal turn a copied
	// full-scope token into this irreversible authority.
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	var request struct {
		Email string `json:"email" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "msg": "invalid revocation request"})
		return
	}
	if err := openvpn.GetManager().RevokeClientEverywhere(request.Email); err != nil {
		logger.Warning("openvpn revocation failed:", err)
		c.JSON(http.StatusConflict, gin.H{"success": false, "msg": "revocation incomplete; verify sessions and accounting before releasing quota"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "msg": "authorization revoked and current sessions confirmed terminated; verify final usage before quota changes"})
}

// BackuptoTgbot sends a full panel backup outside the panel boundary. It is
// intentionally browser-session-only, rather than an ordinary API-token task.
func (a *APIController) BackuptoTgbot(c *gin.Context) {
	if !requireInteractiveSuperAdmin(c) {
		return
	}
	a.Tgbot.SendBackupToAdmins()
}

func isAuditReadOnlyPost(relPath string) bool {
	switch {
	case relPath == "/panel/api/setting/all",
		relPath == "/panel/api/setting/defaultSettings",
		relPath == "/panel/api/xray/",
		relPath == "/panel/api/xray/testOutbound",
		relPath == "/panel/api/xray/testOutbounds",
		relPath == "/panel/api/xray/balancerStatus",
		relPath == "/panel/api/xray/routeTest",
		relPath == "/panel/api/xray/outbound-subs/parse",
		relPath == "/panel/api/inbounds/onlines",
		relPath == "/panel/api/inbounds/lastOnline",
		relPath == "/panel/api/server/checkHost",
		strings.HasPrefix(relPath, "/panel/api/inbounds/clientIps/"),
		strings.HasPrefix(relPath, "/panel/api/server/logs/"),
		strings.HasPrefix(relPath, "/panel/api/server/xraylogs/"),
		strings.HasPrefix(relPath, "/panel/api/server/amneziawglogs/"):
		return true
	default:
		return false
	}
}

func classifyAuditModule(relPath string) string {
	switch {
	case strings.HasPrefix(relPath, "/panel/api/inbounds"):
		return "inbounds"
	case strings.HasPrefix(relPath, "/panel/api/clients"):
		return "clients"
	case strings.HasPrefix(relPath, "/panel/api/resellers"),
		strings.HasPrefix(relPath, "/panel/api/reseller"):
		return "resellers"
	case strings.HasPrefix(relPath, "/panel/api/nodes"):
		return "nodes"
	case strings.HasPrefix(relPath, "/panel/api/xray"):
		return "xray"
	case strings.HasPrefix(relPath, "/panel/api/server"):
		return "server"
	case strings.HasPrefix(relPath, "/panel/api/openvpn"):
		return "openvpn"
	default:
		return "settings"
	}
}

func (a *APIController) auditMiddleware(c *gin.Context) {
	c.Next()

	method := strings.ToUpper(c.Request.Method)
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return
	}
	idx := strings.Index(c.Request.URL.Path, "/panel/api")
	if idx < 0 {
		return
	}
	relPath := c.Request.URL.Path[idx:]
	if isAuditReadOnlyPost(relPath) {
		return
	}

	actorType := "admin"
	actorId := 0
	actorName := "admin"
	if c.GetBool("api_authed") {
		actorType = "api_token"
		actorId = c.GetInt("api_token_id")
		if name := c.GetString("api_token_name"); name != "" {
			actorName = name
		} else {
			actorName = "api_token"
		}
	} else if r := session.GetLoginReseller(c); r != nil {
		actorType = "reseller"
		actorId = r.Id
		actorName = r.Username
	} else if u := session.GetLoginUser(c); u != nil {
		actorType = "admin"
		actorId = u.Id
		actorName = u.Username
	}

	var targets []string
	for _, p := range c.Params {
		if p.Value != "" {
			targets = append(targets, p.Key+"="+p.Value)
		}
	}

	module := classifyAuditModule(relPath)
	a.auditLogService.Record(&model.AuditLog{
		ActorType: actorType,
		ActorId:   actorId,
		ActorName: actorName,
		Action:    method + " " + relPath,
		Module:    module,
		Target:    strings.Join(targets, ", "),
		IP:        getRemoteIp(c),
		Status:    c.Writer.Status(),
	})
	// Never forward request bodies, credentials, or untrusted query data.
	// The durable webhook outbox receives only this redacted audit metadata.
	if c.Writer.Status() < http.StatusBadRequest {
		(service.WebhookService{}).Emit("audit.mutation", map[string]any{"module": module, "method": method, "status": c.Writer.Status(), "actorType": actorType})
	}
}
