package controller

import (
	"context"
	"crypto/subtle"
	"net/http"
	"text/template"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/web/middleware"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/Dark-Sky07/phoenix/internal/web/service/tgbot"
	"github.com/Dark-Sky07/phoenix/internal/web/session"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// LoginForm represents the login request structure.
type LoginForm struct {
	Username      string `json:"username" form:"username"`
	Password      string `json:"password" form:"password"`
	TwoFactorCode string `json:"twoFactorCode" form:"twoFactorCode"`
}

// IndexController handles the main index and login-related routes.
type IndexController struct {
	BaseController

	settingService  service.SettingService
	userService     panel.UserService
	resellerService service.ResellerService
	oidcService     service.OIDCService
	tgbot           tgbot.Tgbot
}

// NewIndexController creates a new IndexController and initializes its routes.
func NewIndexController(g *gin.RouterGroup) *IndexController {
	a := &IndexController{}
	a.initRouter(g)
	return a
}

// initRouter sets up the routes for index, login, logout, and two-factor authentication.
func (a *IndexController) initRouter(g *gin.RouterGroup) {
	g.GET("/", a.index)
	g.GET("/csrf-token", a.csrfToken)

	g.POST("/login", middleware.CSRFMiddleware(), a.login)
	g.POST("/logout", middleware.CSRFMiddleware(), a.logout)
	g.POST("/reauth", middleware.CSRFMiddleware(), a.reauth)
	g.POST("/getTwoFactorEnable", middleware.CSRFMiddleware(), a.getTwoFactorEnable)
	// Generic OIDC uses browser redirects and PKCE. It deliberately lives
	// outside /panel/api so no pre-existing panel session is required.
	g.GET("/sso/oidc/login", a.oidcLogin)
	g.GET("/sso/oidc/callback", a.oidcCallback)
	g.GET("/sso/oidc/status", a.oidcStatus)
}

// index handles the root route, redirecting logged-in users to the panel or showing the login page.
func (a *IndexController) index(c *gin.Context) {
	if session.IsLogin(c) {
		c.Header("Cache-Control", "no-store")
		c.Redirect(http.StatusTemporaryRedirect, c.GetString("base_path")+"panel/")
		return
	}
	serveDistPage(c, "login.html")
}

const (
	oidcStateKey    = "OIDC_STATE"
	oidcNonceKey    = "OIDC_NONCE"
	oidcVerifierKey = "OIDC_VERIFIER"
)

func (a *IndexController) oidcRedirectURI(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host, _ := a.settingService.GetWebDomain()
	if host == "" {
		host = c.Request.Host
	}
	basePath := c.GetString("base_path")
	if basePath == "" {
		basePath = "/"
	}
	return scheme + "://" + host + basePath + "sso/oidc/callback"
}

func (a *IndexController) oidcStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	config, err := a.oidcService.GetConfig()
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"success": true, "obj": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "obj": config.Enabled})
}

func (a *IndexController) oidcLogin(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	begin, err := a.oidcService.Begin(ctx, a.oidcRedirectURI(c))
	if err != nil {
		c.String(http.StatusBadRequest, "OIDC sign-in is unavailable: %v", err)
		return
	}
	s := sessions.Default(c)
	s.Set(oidcStateKey, begin.State)
	s.Set(oidcNonceKey, begin.Nonce)
	s.Set(oidcVerifierKey, begin.CodeVerifier)
	if err := s.Save(); err != nil {
		c.String(http.StatusInternalServerError, "Unable to prepare OIDC sign-in")
		return
	}
	c.Redirect(http.StatusFound, begin.AuthorizationURL)
}

func (a *IndexController) oidcCallback(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	s := sessions.Default(c)
	expectedState, _ := s.Get(oidcStateKey).(string)
	nonce, _ := s.Get(oidcNonceKey).(string)
	verifier, _ := s.Get(oidcVerifierKey).(string)
	s.Delete(oidcStateKey)
	s.Delete(oidcNonceKey)
	s.Delete(oidcVerifierKey)
	_ = s.Save() // consume state even when the provider sends an error

	providedState := c.Query("state")
	if expectedState == "" || subtle.ConstantTimeCompare([]byte(expectedState), []byte(providedState)) != 1 {
		c.String(http.StatusBadRequest, "OIDC sign-in state was invalid or expired")
		return
	}
	if reason := c.Query("error"); reason != "" {
		c.String(http.StatusUnauthorized, "OIDC provider rejected sign-in: %s", reason)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	user, err := a.oidcService.Complete(ctx, a.oidcRedirectURI(c), c.Query("code"), nonce, verifier)
	if err != nil {
		c.String(http.StatusUnauthorized, "OIDC sign-in failed: %v", err)
		return
	}
	if err := session.SetLoginUser(c, user); err != nil {
		c.String(http.StatusInternalServerError, "Unable to create session")
		return
	}
	c.Redirect(http.StatusFound, c.GetString("base_path")+"panel/")
}

// login handles user authentication and session creation.
func (a *IndexController) login(c *gin.Context) {
	var form LoginForm

	if err := c.ShouldBind(&form); err != nil {
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.invalidFormData"))
		return
	}
	if form.Username == "" {
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.emptyUsername"))
		return
	}
	if form.Password == "" {
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.emptyPassword"))
		return
	}

	remoteIP := getRemoteIp(c)
	safeUser := template.HTMLEscapeString(form.Username)
	timeStr := time.Now().Format("2006-01-02 15:04:05")
	if blockedUntil, ok := defaultLoginLimiter.allow(remoteIP, form.Username); !ok {
		reason := "too many failed attempts"
		logger.Warningf("failed login: username=%q, IP=%q, reason=%q, blocked_until=%s", safeUser, remoteIP, reason, blockedUntil.Format(time.RFC3339))
		a.tgbot.UserLoginNotify(tgbot.LoginAttempt{
			Username: safeUser,
			IP:       remoteIP,
			Time:     timeStr,
			Status:   tgbot.LoginFail,
			Reason:   reason,
		})
		emitLoginWebhook(false, safeUser, remoteIP, reason)
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.wrongUsernameOrPassword"))
		return
	}

	user, checkErr := a.userService.CheckUser(form.Username, form.Password, form.TwoFactorCode)

	if user == nil {
		// Not an admin account — maybe a reseller (نمایندگی). Reseller logins
		// are validated against the resellers table and, on success, get a
		// session scoped to their own inbounds and clients.
		if handled := a.loginReseller(c, form, remoteIP, safeUser, timeStr); handled {
			return
		}
	}

	if user == nil {
		reason := loginFailureReason(checkErr)
		if blockedUntil, blocked := defaultLoginLimiter.registerFailure(remoteIP, form.Username); blocked {
			logger.Warningf("failed login: username=%q, IP=%q, reason=%q, blocked_until=%s", safeUser, remoteIP, reason, blockedUntil.Format(time.RFC3339))
		} else {
			logger.Warningf("failed login: username=%q, IP=%q, reason=%q", safeUser, remoteIP, reason)
		}
		a.tgbot.UserLoginNotify(tgbot.LoginAttempt{
			Username: safeUser,
			IP:       remoteIP,
			Time:     timeStr,
			Status:   tgbot.LoginFail,
			Reason:   reason,
		})
		emitLoginWebhook(false, safeUser, remoteIP, reason)
		pureJsonMsg(c, http.StatusOK, false, I18nWeb(c, "pages.login.toasts.wrongUsernameOrPassword"))
		return
	}

	defaultLoginLimiter.registerSuccess(remoteIP, form.Username)
	logger.Infof("%s logged in successfully, Ip Address: %s\n", safeUser, remoteIP)
	a.tgbot.UserLoginNotify(tgbot.LoginAttempt{
		Username: safeUser,
		IP:       remoteIP,
		Time:     timeStr,
		Status:   tgbot.LoginSuccess,
	})
	emitLoginWebhook(true, safeUser, remoteIP, "")

	if err := session.SetLoginUser(c, user); err != nil {
		logger.Warning("Unable to save session:", err)
		return
	}

	logger.Infof("%s logged in successfully", safeUser)
	jsonMsg(c, I18nWeb(c, "pages.login.toasts.successLogin"), nil)
}

// loginReseller authenticates a reseller (نمایندگی) login and, on success,
// stores the reseller session and writes the response. It reports whether it
// handled the request; false means "no such reseller, keep going with the
// regular failure path". A correct password on a disabled/expired account is
// answered with the specific reason instead of a generic failure, and counts
// as a failed attempt for the login limiter.
func (a *IndexController) loginReseller(c *gin.Context, form LoginForm, remoteIP string, safeUser string, timeStr string) bool {
	reseller, err := a.resellerService.CheckReseller(form.Username, form.Password)
	if err != nil {
		if err.Error() == "invalid credentials" {
			return false
		}
		// Correct password, unusable account (disabled or expired).
		reason := err.Error()
		defaultLoginLimiter.registerFailure(remoteIP, form.Username)
		logger.Warningf("failed login: username=%q, IP=%q, reason=%q", safeUser, remoteIP, reason)
		a.tgbot.UserLoginNotify(tgbot.LoginAttempt{
			Username: safeUser,
			IP:       remoteIP,
			Time:     timeStr,
			Status:   tgbot.LoginFail,
			Reason:   reason,
		})
		emitLoginWebhook(false, safeUser, remoteIP, reason)
		pureJsonMsg(c, http.StatusOK, false, err.Error())
		return true
	}

	defaultLoginLimiter.registerSuccess(remoteIP, form.Username)
	logger.Infof("%s (reseller) logged in successfully, Ip Address: %s\n", safeUser, remoteIP)
	a.tgbot.UserLoginNotify(tgbot.LoginAttempt{
		Username: safeUser,
		IP:       remoteIP,
		Time:     timeStr,
		Status:   tgbot.LoginSuccess,
	})
	emitLoginWebhook(true, safeUser, remoteIP, "")

	if err := session.SetLoginReseller(c, reseller); err != nil {
		logger.Warning("Unable to save reseller session:", err)
		return true
	}
	jsonMsg(c, I18nWeb(c, "pages.login.toasts.successLogin"), nil)
	return true
}

func emitLoginWebhook(success bool, username, ip, reason string) {
	event := "security.login.failure"
	if success {
		event = "security.login.success"
	}
	// Deliberately no password, TOTP value, recovery code, cookie or request
	// payload is ever forwarded to an event subscriber.
	(service.WebhookService{}).Emit(event, map[string]any{"username": username, "ip": ip, "reason": reason})
}

func loginFailureReason(err error) string {
	if err != nil && err.Error() == "invalid 2fa code" {
		return "invalid 2FA code"
	}
	return "invalid credentials"
}

// reauth proves that the person holding an existing browser session still
// knows the primary credential (and TOTP/recovery code when configured). High
// impact endpoints can require this short-lived proof without accepting API
// bearer tokens as an equivalent credential.
func (a *IndexController) reauth(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user == nil {
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	var form struct {
		Password      string `json:"password" form:"password"`
		TwoFactorCode string `json:"twoFactorCode" form:"twoFactorCode"`
	}
	if err := c.ShouldBind(&form); err != nil || form.Password == "" {
		pureJsonMsg(c, http.StatusBadRequest, false, "password is required")
		return
	}
	confirmed, err := a.userService.CheckUser(user.Username, form.Password, form.TwoFactorCode)
	if err != nil || confirmed == nil || confirmed.Id != user.Id {
		pureJsonMsg(c, http.StatusUnauthorized, false, "reauthentication failed")
		return
	}
	session.SetRecentAuthentication(c)
	if err := sessions.Default(c).Save(); err != nil {
		pureJsonMsg(c, http.StatusInternalServerError, false, "unable to save authentication")
		return
	}
	jsonMsg(c, "reauthentication successful", nil)
}

func (a *IndexController) logout(c *gin.Context) {
	user := session.GetLoginUser(c)
	if user != nil {
		logger.Infof("%s logged out successfully", user.Username)
	}
	if err := session.ClearSession(c); err != nil {
		logger.Warning("Unable to clear session on logout:", err)
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"success": true})
}

// csrfToken returns the session CSRF token. Public — the login page
// needs a token before authenticating.
func (a *IndexController) csrfToken(c *gin.Context) {
	token, err := session.EnsureCSRFToken(c)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "msg": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "obj": token})
}

// getTwoFactorEnable retrieves the current status of two-factor authentication.
func (a *IndexController) getTwoFactorEnable(c *gin.Context) {
	status, err := a.settingService.GetTwoFactorEnable()
	if err == nil {
		jsonObj(c, status, nil)
	}
}
