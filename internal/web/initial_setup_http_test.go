package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/global"
	"github.com/Dark-Sky07/phoenix/internal/web/service"

	"github.com/robfig/cron/v3"
)

func newInitialSetupTestRouter(t *testing.T) http.Handler {
	t.Helper()
	server := NewServer()
	server.cron = cron.New()
	global.SetWebServer(server)
	t.Cleanup(func() {
		server.cron.Stop()
		server.cancel()
		global.SetWebServer(nil)
	})
	engine, err := server.initRouter()
	if err != nil {
		t.Fatalf("init router: %v", err)
	}
	return engine
}

func TestInitialSetupRouterFreshBasePathLifecycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	if err := (&service.SettingService{}).SetBasePath("/bootstrap/"); err != nil {
		t.Fatal(err)
	}

	engine := newInitialSetupTestRouter(t)
	tokenPath := filepath.Join(dir, "initial-setup-token")
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read local setup token: %v", err)
	}
	info, err := os.Stat(tokenPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("setup-token permissions = %v, err = %v; want 0600", info.Mode().Perm(), err)
	}
	token := strings.TrimSpace(string(tokenBytes))

	setup := httptest.NewRecorder()
	engine.ServeHTTP(setup, httptest.NewRequest(http.MethodGet, "/bootstrap/setup", nil))
	if setup.Code != http.StatusOK || !strings.Contains(setup.Body.String(), "Create the first administrator") {
		t.Fatalf("fresh setup form = (%d, %q)", setup.Code, setup.Body.String())
	}
	if strings.Contains(setup.Body.String(), token) {
		t.Fatal("setup form must never expose its one-time token")
	}

	blocked := httptest.NewRecorder()
	engine.ServeHTTP(blocked, httptest.NewRequest(http.MethodGet, "/bootstrap/panel/", nil))
	if blocked.Code != http.StatusNotFound {
		t.Fatalf("unconfigured management route = %d, want 404", blocked.Code)
	}

	form := url.Values{
		"token":           {token},
		"username":        {"first-admin"},
		"password":        {"a-long-first-password"},
		"confirmPassword": {"a-long-first-password"},
	}
	complete := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/bootstrap/setup", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	engine.ServeHTTP(complete, req)
	if complete.Code != http.StatusOK || !strings.Contains(complete.Body.String(), "Initial administrator created") {
		t.Fatalf("setup completion = (%d, %q)", complete.Code, complete.Body.String())
	}
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("consumed setup token file remains: %v", err)
	}
	var user model.User
	if err := database.GetDB().Where("username = ?", "first-admin").First(&user).Error; err != nil || user.Role != "superadmin" {
		t.Fatalf("first administrator = %#v, err=%v", user, err)
	}
}

func TestInitialSetupRouterUpgradeDoesNotCreateToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	existing := &model.User{Username: "existing-admin", Password: "existing-hash", Role: "superadmin", LoginEpoch: 9}
	if err := database.GetDB().Create(existing).Error; err != nil {
		t.Fatal(err)
	}

	engine := newInitialSetupTestRouter(t)
	if _, err := os.Stat(filepath.Join(dir, "initial-setup-token")); !os.IsNotExist(err) {
		t.Fatalf("upgrade unexpectedly created setup token: %v", err)
	}
	setup := httptest.NewRecorder()
	engine.ServeHTTP(setup, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if setup.Code != http.StatusFound {
		t.Fatalf("upgrade setup route status = %d, want %d", setup.Code, http.StatusFound)
	}
	var got model.User
	if err := database.GetDB().First(&got, existing.Id).Error; err != nil || got.Username != existing.Username || got.LoginEpoch != existing.LoginEpoch {
		t.Fatalf("upgrade modified existing admin: %#v, err=%v", got, err)
	}
}
