package controller

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/gin-gonic/gin"
)

// InitialSetupController is a deliberately small server-rendered page. A
// fresh panel has no account with which to authenticate to the normal SPA.
type InitialSetupController struct{ setup service.InitialSetupService }

func NewInitialSetupController(g *gin.RouterGroup) *InitialSetupController {
	a := &InitialSetupController{}
	g.GET("/setup", a.page)
	g.POST("/setup", a.complete)
	return a
}

func (a *InitialSetupController) required() bool {
	required, err := a.setup.Required()
	return err == nil && required
}

// Gate prevents an unconfigured panel from exposing management, subscription,
// or checkout surfaces. Established installations never enter this path.
func (a *InitialSetupController) Gate(c *gin.Context) {
	if !a.required() {
		c.Next()
		return
	}
	path := strings.TrimSuffix(c.Request.URL.Path, "/")
	base := strings.TrimSuffix(c.GetString("base_path"), "/")
	if path == base+"/setup" || strings.HasPrefix(path, base+"/assets/") || strings.HasSuffix(path, "/manifest.webmanifest") || strings.HasSuffix(path, "/sw.js") {
		c.Next()
		return
	}
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatus(http.StatusNotFound)
}

func (a *InitialSetupController) page(c *gin.Context) {
	if !a.required() {
		c.Redirect(http.StatusFound, c.GetString("base_path"))
		return
	}
	a.render(c, "", false)
}

func (a *InitialSetupController) complete(c *gin.Context) {
	if !a.required() {
		c.Redirect(http.StatusFound, c.GetString("base_path"))
		return
	}
	password := c.PostForm("password")
	if password != c.PostForm("confirmPassword") {
		a.render(c, "Passwords do not match", false)
		return
	}
	if err := a.setup.Complete(c.PostForm("token"), c.PostForm("username"), password); err != nil {
		a.render(c, err.Error(), false)
		return
	}
	a.setup.Finish()
	a.render(c, "Initial administrator created. You can now sign in.", true)
}

func (a *InitialSetupController) render(c *gin.Context, message string, success bool) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = setupPageTemplate.Execute(c.Writer, gin.H{"Message": message, "Success": success})
}

var setupPageTemplate = template.Must(template.New("setup").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>PHOENIX Initial setup</title><style>body{margin:0;background:#111827;color:#f9fafb;font:16px system-ui;display:grid;min-height:100vh;place-items:center}.card{background:#1f2937;border:1px solid #374151;border-radius:16px;padding:32px;width:min(420px,calc(100% - 48px))}h1{margin-top:0;color:#fbbf24}label{display:block;margin:16px 0 6px}input{box-sizing:border-box;width:100%;padding:11px;border-radius:8px;border:1px solid #4b5563;background:#111827;color:#fff}button{margin-top:24px;width:100%;padding:12px;border:0;border-radius:8px;background:#f59e0b;color:#111827;font-weight:700}.error{color:#fca5a5}.ok{color:#86efac}</style></head><body><main class="card">{{if .Success}}<h1>PHOENIX is ready</h1><p class="ok">{{.Message}}</p><a href="../">Open the login page</a>{{else}}<h1>Create the first administrator</h1><p>Read the one-time token from the protected <code>initial-setup-token</code> file on this server. It expires shortly and is never sent in a URL.</p>{{if .Message}}<p class="error">{{.Message}}</p>{{end}}<form method="post" action="setup"><label>Setup token</label><input name="token" autocomplete="off" minlength="48" maxlength="48" required autofocus><label>Username</label><input name="username" autocomplete="username" minlength="3" maxlength="64" required><label>Password</label><input type="password" name="password" autocomplete="new-password" minlength="12" required><label>Confirm password</label><input type="password" name="confirmPassword" autocomplete="new-password" minlength="12" required><button type="submit">Create secure administrator</button></form>{{end}}</main></body></html>`))
