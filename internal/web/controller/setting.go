package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"
	"github.com/Dark-Sky07/phoenix/internal/web/entity"
	"github.com/Dark-Sky07/phoenix/internal/web/global"
	"github.com/Dark-Sky07/phoenix/internal/web/middleware"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/Dark-Sky07/phoenix/internal/web/session"

	"github.com/gin-gonic/gin"
)

// updateUserForm represents the form for updating user credentials.
type updateUserForm struct {
	OldUsername string `json:"oldUsername" form:"oldUsername"`
	OldPassword string `json:"oldPassword" form:"oldPassword"`
	NewUsername string `json:"newUsername" form:"newUsername"`
	NewPassword string `json:"newPassword" form:"newPassword"`
}

// SettingController handles settings and user management operations.
type SettingController struct {
	settingService        service.SettingService
	userService           panel.UserService
	panelService          panel.PanelService
	apiTokenService       panel.ApiTokenService
	xrayService           service.XrayService
	certService           service.CertService
	discordService        service.DiscordService
	cloudBackupService    service.CloudBackupService
	auditLogService       service.AuditLogService
	dnsCertificateService service.DNSCertificateService
	oidcService           service.OIDCService
}

// NewSettingController creates a new SettingController and initializes its routes.
func NewSettingController(g *gin.RouterGroup) *SettingController {
	a := &SettingController{}
	a.initRouter(g)
	return a
}

// initRouter sets up the routes for settings management.
func (a *SettingController) initRouter(g *gin.RouterGroup) {
	g = g.Group("/setting")

	g.POST("/all", a.getAllSetting)
	g.POST("/defaultSettings", a.getDefaultSettings)
	g.POST("/update", a.updateSetting)
	g.POST("/updateUser", a.updateUser)
	g.POST("/restartPanel", a.restartPanel)
	g.GET("/getDefaultJsonConfig", a.getDefaultXrayConfig)
	g.GET("/apiTokens", a.listApiTokens)
	g.POST("/apiTokens/create", a.createApiToken)
	g.POST("/apiTokens/delete/:id", a.deleteApiToken)
	g.POST("/apiTokens/setEnabled/:id", a.setApiTokenEnabled)
	g.GET("/sessions", a.listSessions)
	g.POST("/sessions/revoke/:id", a.revokeSession)
	g.POST("/recoveryCodes/regenerate", a.regenerateRecoveryCodes)
	g.GET("/webhooks", a.listWebhooks)
	g.POST("/webhooks/create", a.createWebhook)
	g.POST("/webhooks/delete/:id", a.deleteWebhook)
	g.POST("/webhooks/setEnabled/:id", a.setWebhookEnabled)
	g.GET("/webhooks/deliveries", a.listWebhookDeliveries)
	g.POST("/webhooks/deliveries/:id/replay", a.replayWebhookDelivery)
	g.GET("/cert/status", a.getCertStatus)
	g.POST("/cert/issue", a.issueCert)
	g.GET("/cert/dns-profiles", a.listDNSCertificateProfiles)
	g.POST("/cert/dns-profiles", a.createDNSCertificateProfile)
	g.POST("/cert/dns-profiles/:id", a.updateDNSCertificateProfile)
	g.POST("/cert/dns-profiles/:id/issue", a.issueDNSCertificateProfile)
	g.POST("/cert/dns-profiles/:id/complete", a.completeDNSCertificateProfile)
	g.POST("/cert/dns-profiles/:id/delete", a.deleteDNSCertificateProfile)
	g.GET("/oidc", a.getOIDCConfig)
	g.POST("/oidc", a.saveOIDCConfig)
	g.POST("/discord/test", a.testDiscord)
	g.POST("/cloudBackup/run", a.runCloudBackup)
	g.GET("/auditLogs", a.listAuditLogs)
	g.POST("/auditLogs/clear", a.clearAuditLogs)
	g.GET("/admins", a.listAdmins)
	g.POST("/admins/create", a.createAdmin)
	g.POST("/admins/update/:id", a.updateAdmin)
	g.POST("/admins/delete/:id", a.deleteAdmin)
}

// getAllSetting retrieves all current settings.
func (a *SettingController) getAllSetting(c *gin.Context) {
	allSetting, err := a.settingService.GetAllSetting()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.getSettings"), err)
		return
	}
	jsonObj(c, allSetting, nil)
}

// getDefaultSettings retrieves the default settings based on the host.
func (a *SettingController) getDefaultSettings(c *gin.Context) {
	result, err := a.settingService.GetDefaultSettings(c.Request.Host)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.getSettings"), err)
		return
	}
	jsonObj(c, result, nil)
}

func preserveBrandSettings(submitted map[string]json.RawMessage, current *entity.AllSetting) {
	if submitted == nil || current == nil {
		return
	}
	values := map[string]any{
		"brandName": current.BrandName, "brandLogo": current.BrandLogo,
		"brandColor": current.BrandColor, "brandTheme": current.BrandTheme,
		"subBrandName": current.SubBrandName, "subBrandLogo": current.SubBrandLogo,
		"subBrandColor": current.SubBrandColor, "subBrandTheme": current.SubBrandTheme,
	}
	for key, value := range values {
		if _, exists := submitted[key]; exists {
			continue
		}
		if raw, err := json.Marshal(value); err == nil {
			submitted[key] = raw
		}
	}
}

func preservePanelRestartSettings(submitted map[string]json.RawMessage, current *entity.AllSetting) {
	if submitted == nil || current == nil {
		return
	}
	values := map[string]any{
		"panelAutoRestartEnable": current.PanelAutoRestartEnable,
		"panelAutoRestartCron":   current.PanelAutoRestartCron,
	}
	for key, value := range values {
		if _, exists := submitted[key]; exists {
			continue
		}
		encoded, _ := json.Marshal(value)
		submitted[key] = encoded
	}
}

func preserveResellerNotificationSettings(submitted map[string]json.RawMessage, current *entity.AllSetting) {
	if submitted == nil || current == nil {
		return
	}
	defaults := map[string]any{
		"resellerNotifTrafficWarningPercent":  current.ResellerNotifTrafficWarningPercent,
		"resellerNotifTrafficCriticalPercent": current.ResellerNotifTrafficCriticalPercent,
		"resellerNotifClientWarningPercent":   current.ResellerNotifClientWarningPercent,
		"resellerNotifExpiryWarningDays":      current.ResellerNotifExpiryWarningDays,
		"resellerNotifInPanelEnable":          current.ResellerNotifInPanelEnable,
		"resellerNotifTelegramEnable":         current.ResellerNotifTelegramEnable,
	}
	for key, value := range defaults {
		if _, exists := submitted[key]; !exists {
			encoded, _ := json.Marshal(value)
			submitted[key] = encoded
		}
	}
}

// updateSetting updates all settings with the provided data.
func (a *SettingController) updateSetting(c *gin.Context) {
	// Older panel versions submit a full settings object without reseller
	// notification fields. Fill only absent fields from persisted/default
	// settings so those updates cannot silently reset newer preferences.
	if c.Request.Body != nil {
		body, readErr := io.ReadAll(c.Request.Body)
		if readErr != nil {
			jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), readErr)
			return
		}
		var submitted map[string]json.RawMessage
		if json.Unmarshal(body, &submitted) == nil && submitted != nil {
			current, err := a.settingService.GetAllSetting()
			if err != nil {
				jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
				return
			}
			preserveResellerNotificationSettings(submitted, current)
			preserveBrandSettings(submitted, current)
			preservePanelRestartSettings(submitted, current)
			body, _ = json.Marshal(submitted)
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
	}
	allSetting, ok := middleware.BindAndValidate[entity.AllSetting](c)
	if !ok {
		return
	}
	oldTwoFactor, twoFactorErr := a.settingService.GetTwoFactorEnable()
	oldPanelOutbound, _ := a.settingService.GetPanelOutbound()
	err := a.settingService.UpdateAllSetting(allSetting)
	if err == nil && twoFactorErr == nil && !oldTwoFactor && allSetting.TwoFactorEnable {
		if bumpErr := a.userService.BumpLoginEpoch(); bumpErr != nil {
			err = bumpErr
		}
	}
	if err == nil && allSetting.PanelOutbound != oldPanelOutbound {
		// The egress bridge lives in the generated config; reconcile the
		// running core. One SOCKS inbound plus one routing rule — both
		// hot-appliable, so this normally does not restart Xray.
		if applyErr := a.xrayService.RestartXray(false); applyErr != nil {
			logger.Warning("apply panel outbound change failed:", applyErr)
		}
	}
	if err == nil {
		if server := global.GetWebServer(); server != nil {
			if scheduler, ok := server.(interface {
				SyncPanelRestartSchedule(bool, string) error
			}); ok {
				if syncErr := scheduler.SyncPanelRestartSchedule(allSetting.PanelAutoRestartEnable, allSetting.PanelAutoRestartCron); syncErr != nil {
					logger.Warning("apply panel restart schedule failed:", syncErr)
					err = syncErr
				}
			}
		}
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
}

// updateUser updates the current user's username and password.
func (a *SettingController) updateUser(c *gin.Context) {
	form := &updateUserForm{}
	err := c.ShouldBind(form)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	user := session.GetLoginUser(c)
	if user.Username != form.OldUsername || !crypto.CheckPasswordHash(user.Password, form.OldPassword) {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifyUserError"), errors.New(I18nWeb(c, "pages.settings.toasts.originalUserPassIncorrect")))
		return
	}
	if form.NewUsername == "" || form.NewPassword == "" {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifyUserError"), errors.New(I18nWeb(c, "pages.settings.toasts.userPassMustBeNotEmpty")))
		return
	}
	err = a.userService.UpdateUser(user.Id, form.NewUsername, form.NewPassword)
	if err == nil {
		user.Username = form.NewUsername
		user.Password, _ = crypto.HashPasswordAsBcrypt(form.NewPassword)
		if saveErr := session.SetLoginUser(c, user); saveErr != nil {
			err = saveErr
		}
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifyUser"), err)
}

// restartPanel restarts the panel service after a delay.
func (a *SettingController) restartPanel(c *gin.Context) {
	err := a.panelService.RestartPanel(time.Second * 3)
	jsonMsg(c, I18nWeb(c, "pages.settings.restartPanelSuccess"), err)
}

// getDefaultXrayConfig retrieves the default Xray configuration.
func (a *SettingController) getDefaultXrayConfig(c *gin.Context) {
	defaultJsonConfig, err := a.settingService.GetDefaultXrayConfig()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.getSettings"), err)
		return
	}
	jsonObj(c, defaultJsonConfig, nil)
}

func (a *SettingController) listSessions(c *gin.Context) {
	if !requireInteractiveSuperAdmin(c) {
		return
	}
	user := session.GetLoginUser(c)
	rows, err := session.ListTrackedSessions(user.Id)
	jsonObj(c, rows, err)
}

func (a *SettingController) revokeSession(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err == nil {
		err = session.RevokeTrackedSession(session.GetLoginUser(c).Id, id)
	}
	jsonMsg(c, "session revoked", err)
}

// regenerateRecoveryCodes returns plaintext codes once and never persists or
// logs them. A code can be used in the normal 2FA field if the authenticator is
// unavailable; it is consumed atomically.
func (a *SettingController) regenerateRecoveryCodes(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	codes, err := a.userService.GenerateRecoveryCodes(session.GetLoginUser(c).Id)
	jsonObj(c, gin.H{"codes": codes}, err)
}

type webhookCreateForm struct {
	Name   string   `json:"name" form:"name"`
	URL    string   `json:"url" form:"url"`
	Events []string `json:"events" form:"events"`
}

func (a *SettingController) listWebhooks(c *gin.Context) {
	if !requireInteractiveSuperAdmin(c) {
		return
	}
	rows, err := (service.WebhookService{}).List()
	jsonObj(c, rows, err)
}

func (a *SettingController) createWebhook(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	var form webhookCreateForm
	if err := c.ShouldBind(&form); err != nil {
		jsonMsg(c, "invalid webhook form", err)
		return
	}
	row, err := (service.WebhookService{}).Create(form.Name, form.URL, form.Events)
	jsonObj(c, row, err)
}

func (a *SettingController) deleteWebhook(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err == nil {
		err = (service.WebhookService{}).Delete(id)
	}
	jsonMsg(c, "webhook deleted", err)
}

func (a *SettingController) setWebhookEnabled(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	var form apiTokenEnabledForm
	if err == nil {
		err = c.ShouldBind(&form)
	}
	if err == nil {
		err = (service.WebhookService{}).SetEnabled(id, form.Enabled)
	}
	jsonMsg(c, "webhook updated", err)
}

func (a *SettingController) listWebhookDeliveries(c *gin.Context) {
	if !requireInteractiveSuperAdmin(c) {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	rows, err := (service.WebhookService{}).ListDeliveries(limit)
	jsonObj(c, rows, err)
}

func (a *SettingController) replayWebhookDelivery(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err == nil {
		err = (service.WebhookService{}).Replay(id)
	}
	jsonMsg(c, "webhook delivery queued", err)
}

type apiTokenCreateForm struct {
	Name       string   `json:"name" form:"name"`
	Scope      string   `json:"scope" form:"scope"`
	Scopes     []string `json:"scopes" form:"scopes"`
	ExpireDays int      `json:"expireDays" form:"expireDays" validate:"gte=0"`
}

type apiTokenEnabledForm struct {
	Enabled bool `json:"enabled" form:"enabled"`
}

func (a *SettingController) listApiTokens(c *gin.Context) {
	if !a.ensureSuperAdmin(c) {
		return
	}
	rows, err := a.apiTokenService.List()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.getSettings"), err)
		return
	}
	jsonObj(c, rows, nil)
}

func (a *SettingController) createApiToken(c *gin.Context) {
	if !a.ensureRecentSuperAdmin(c) {
		return
	}
	form := &apiTokenCreateForm{}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	var row *panel.ApiTokenView
	var err error
	if len(form.Scopes) > 0 {
		row, err = a.apiTokenService.CreateWithPermissions(form.Name, form.Scopes, form.ExpireDays)
	} else {
		row, err = a.apiTokenService.CreateWithOptions(form.Name, form.Scope, form.ExpireDays)
	}
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	jsonObj(c, row, nil)
}

func (a *SettingController) deleteApiToken(c *gin.Context) {
	if !a.ensureRecentSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), a.apiTokenService.Delete(id))
}

func (a *SettingController) setApiTokenEnabled(c *gin.Context) {
	if !a.ensureRecentSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	form := &apiTokenEnabledForm{}
	if bindErr := c.ShouldBind(form); bindErr != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), bindErr)
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), a.apiTokenService.SetEnabled(id, form.Enabled))
}

type certIssueForm struct {
	Domain       string `json:"domain" form:"domain"`
	Email        string `json:"email" form:"email"`
	Mode         string `json:"mode" form:"mode"` // "letsencrypt" (default) or "selfsigned"
	ApplyToPanel bool   `json:"applyToPanel" form:"applyToPanel"`
}

func (a *SettingController) getCertStatus(c *gin.Context) {
	jsonObj(c, a.certService.CurrentStatus(), nil)
}

func (a *SettingController) issueCert(c *gin.Context) {
	if !a.ensureSuperAdmin(c) {
		return
	}
	form := &certIssueForm{ApplyToPanel: true}
	if err := c.ShouldBind(form); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	var (
		st  *service.CertStatus
		err error
	)
	if strings.EqualFold(strings.TrimSpace(form.Mode), "selfsigned") {
		st, err = a.certService.IssueSelfSigned(form.Domain, "", form.ApplyToPanel)
	} else {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Minute)
		defer cancel()
		st, err = a.certService.IssueLetsEncrypt(ctx, form.Domain, form.Email, "", form.ApplyToPanel)
	}
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	jsonObj(c, st, nil)
}

func (a *SettingController) listDNSCertificateProfiles(c *gin.Context) {
	if !requireInteractiveSuperAdmin(c) {
		return
	}
	rows, err := a.dnsCertificateService.List()
	jsonObj(c, rows, err)
}

func (a *SettingController) createDNSCertificateProfile(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	var input service.DNSCertificateProfileInput
	if err := c.ShouldBindJSON(&input); err != nil {
		jsonMsg(c, "invalid DNS certificate profile", err)
		return
	}
	row, err := a.dnsCertificateService.Create(input)
	jsonObj(c, row, err)
}

func (a *SettingController) updateDNSCertificateProfile(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "invalid DNS certificate profile id", err)
		return
	}
	var input service.DNSCertificateProfileInput
	if err := c.ShouldBindJSON(&input); err != nil {
		jsonMsg(c, "invalid DNS certificate profile", err)
		return
	}
	row, err := a.dnsCertificateService.Update(id, input)
	jsonObj(c, row, err)
}

func (a *SettingController) issueDNSCertificateProfile(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "invalid DNS certificate profile id", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Minute)
	defer cancel()
	result, err := a.dnsCertificateService.Issue(ctx, id)
	jsonObj(c, result, err)
}

func (a *SettingController) completeDNSCertificateProfile(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		jsonMsg(c, "invalid DNS certificate profile id", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Minute)
	defer cancel()
	result, err := a.dnsCertificateService.CompleteManual(ctx, id)
	jsonObj(c, result, err)
}

func (a *SettingController) deleteDNSCertificateProfile(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err == nil {
		err = a.dnsCertificateService.Delete(id)
	}
	jsonMsg(c, "DNS certificate profile deleted", err)
}

func (a *SettingController) getOIDCConfig(c *gin.Context) {
	if !requireInteractiveSuperAdmin(c) {
		return
	}
	view, err := a.oidcService.GetConfig()
	jsonObj(c, view, err)
}

func (a *SettingController) saveOIDCConfig(c *gin.Context) {
	if !requireRecentInteractiveSuperAdmin(c) {
		return
	}
	var input service.OIDCConfigInput
	if err := c.ShouldBindJSON(&input); err != nil {
		jsonMsg(c, "invalid OIDC configuration", err)
		return
	}
	view, err := a.oidcService.SaveConfig(input)
	jsonObj(c, view, err)
}

func (a *SettingController) testDiscord(c *gin.Context) {
	jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), a.discordService.SendTestMessage())
}

func (a *SettingController) runCloudBackup(c *gin.Context) {
	if !a.ensureSuperAdmin(c) {
		return
	}
	key, err := a.cloudBackupService.BackupNow()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	jsonObj(c, gin.H{"key": key}, nil)
}

func (a *SettingController) listAuditLogs(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	actorType := c.Query("actorType")
	module := c.Query("module")
	search := c.Query("search")
	logs, err := a.auditLogService.List(limit, actorType, module, search)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.getSettings"), err)
		return
	}
	jsonObj(c, logs, nil)
}

func (a *SettingController) clearAuditLogs(c *gin.Context) {
	if !a.ensureSuperAdmin(c) {
		return
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), a.auditLogService.Clear())
}

type adminForm struct {
	Username    string   `json:"username" form:"username"`
	Password    string   `json:"password" form:"password"`
	Role        string   `json:"role" form:"role"`
	Permissions []string `json:"permissions" form:"permissions"`
	Note        string   `json:"note" form:"note"`
	Disabled    bool     `json:"disabled" form:"disabled"`
}

func (a *SettingController) ensureSuperAdmin(c *gin.Context) bool {
	return requireInteractiveSuperAdmin(c)
}

func (a *SettingController) ensureRecentSuperAdmin(c *gin.Context) bool {
	return requireRecentInteractiveSuperAdmin(c)
}

func (a *SettingController) listAdmins(c *gin.Context) {
	if !a.ensureSuperAdmin(c) {
		return
	}
	admins, err := a.userService.ListAdmins()
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.getSettings"), err)
		return
	}
	jsonObj(c, admins, nil)
}

func (a *SettingController) createAdmin(c *gin.Context) {
	if !a.ensureRecentSuperAdmin(c) {
		return
	}
	var form adminForm
	if err := c.ShouldBindJSON(&form); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	created, err := a.userService.CreateAdmin(form.Username, form.Password, form.Role, form.Permissions, form.Note)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	jsonObj(c, created, nil)
}

func (a *SettingController) updateAdmin(c *gin.Context) {
	if !a.ensureRecentSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), errors.New("invalid admin id"))
		return
	}
	var form adminForm
	if err := c.ShouldBindJSON(&form); err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	updated, err := a.userService.UpdateAdmin(id, form.Username, form.Password, form.Role, form.Permissions, form.Note, form.Disabled)
	if err != nil {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), err)
		return
	}
	jsonObj(c, updated, nil)
}

func (a *SettingController) deleteAdmin(c *gin.Context) {
	if !a.ensureRecentSuperAdmin(c) {
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), errors.New("invalid admin id"))
		return
	}
	callerId := 0
	if u := session.GetLoginUser(c); u != nil {
		callerId = u.Id
	}
	jsonMsg(c, I18nWeb(c, "pages.settings.toasts.modifySettings"), a.userService.DeleteAdmin(id, callerId))
}
