package controller

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func initPrometheusTestDB(t *testing.T) panel.ApiTokenService {
	t.Helper()
	dbDir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dbDir)
	if err := database.InitDB(filepath.Join(dbDir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	return panel.ApiTokenService{}
}

func TestPrometheusMetricsAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tokens := initPrometheusTestDB(t)
	metricsToken, err := tokens.CreateWithPermissions("metrics", []string{"metrics:read"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := tokens.CreateWithPermissions("other", []string{"clients:read"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	controller := &APIController{apiTokenService: tokens}
	for _, test := range []struct {
		name  string
		token string
		want  int
	}{
		{name: "anonymous", want: http.StatusUnauthorized},
		{name: "wrong permission", token: otherToken.Token, want: http.StatusUnauthorized},
		{name: "metrics permission", token: metricsToken.Token, want: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.Use(sessions.Sessions("phoenix", cookie.NewStore([]byte("prometheus-test-session-key"))))
			router.GET("/metrics", controller.prometheusMetrics)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
			if test.token != "" {
				req.Header.Set("Authorization", "Bearer "+test.token)
			}
			router.ServeHTTP(recorder, req)
			if recorder.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

func TestRenderPrometheusMetricsUsesCounterMetadata(t *testing.T) {
	status := &service.Status{}
	status.NetIO.Up, status.NetIO.Down = 42, 7
	status.Xray.State = service.Running
	body := renderPrometheusMetrics(status)
	for _, want := range []string{
		"# TYPE phoenix_network_bytes_total counter",
		"phoenix_network_bytes_total{direction=\"transmit\"} 42",
		"phoenix_process_up{service=\"xray\"} 1",
		"# HELP phoenix_catalog_entries Cached entries in enabled rule-set catalogs",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics response does not contain %q:\n%s", want, body)
		}
	}
}
