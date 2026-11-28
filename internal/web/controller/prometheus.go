package controller

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/service/panel"
	"github.com/Dark-Sky07/phoenix/internal/web/session"

	"github.com/gin-gonic/gin"
)

// prometheusMetrics exposes a deliberately small, stable scrape surface. It
// is not public: use a legacy full/read API token or a granular metrics:read
// token as a Prometheus bearer token. This avoids turning panel and subscriber
// metadata into anonymous internet-visible information.
func (a *APIController) prometheusMetrics(c *gin.Context) {
	if !allowPrometheusMetrics(c, &a.apiTokenService) {
		c.Header("WWW-Authenticate", `Bearer realm="PHOENIX metrics"`)
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	if a.serverController == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	status := a.serverController.serverService.LastStatus()
	if status == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	_, _ = c.Writer.WriteString(renderPrometheusMetrics(status))
}

func allowPrometheusMetrics(c *gin.Context, tokens *panel.ApiTokenService) bool {
	if strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ") {
		presented := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		if token, ok := tokens.Authenticate(presented); ok {
			return panel.TokenAllowsRequest(token, http.MethodGet, "/panel/api/metrics")
		}
		return false
	}
	return session.GetLoginUser(c) != nil
}

func renderPrometheusMetrics(status *service.Status) string {
	var b strings.Builder
	writeMetricHelp(&b, "phoenix_up", "Whether PHOENIX has produced a current health snapshot")
	writeMetric(&b, "phoenix_up", 1)
	writeMetricHelp(&b, "phoenix_cpu_usage_percent", "Host CPU utilization percentage")
	writeMetric(&b, "phoenix_cpu_usage_percent", status.Cpu)
	writeMetricHelp(&b, "phoenix_memory_bytes", "Host memory usage in bytes")
	writeMetric(&b, "phoenix_memory_bytes", float64(status.Mem.Current), `state="used"`)
	writeMetric(&b, "phoenix_memory_bytes", float64(status.Mem.Total), `state="total"`)
	writeMetricHelp(&b, "phoenix_swap_bytes", "Host swap usage in bytes")
	writeMetric(&b, "phoenix_swap_bytes", float64(status.Swap.Current), `state="used"`)
	writeMetric(&b, "phoenix_swap_bytes", float64(status.Swap.Total), `state="total"`)
	writeMetricHelp(&b, "phoenix_disk_bytes", "Host disk usage in bytes")
	writeMetric(&b, "phoenix_disk_bytes", float64(status.Disk.Current), `state="used"`)
	writeMetric(&b, "phoenix_disk_bytes", float64(status.Disk.Total), `state="total"`)
	writeMetricCounterHelp(&b, "phoenix_network_bytes_total", "Host network byte counters")
	writeMetric(&b, "phoenix_network_bytes_total", float64(status.NetIO.Up), `direction="transmit"`)
	writeMetric(&b, "phoenix_network_bytes_total", float64(status.NetIO.Down), `direction="receive"`)
	writeMetricCounterHelp(&b, "phoenix_network_packets_total", "Host network packet counters")
	writeMetric(&b, "phoenix_network_packets_total", float64(status.NetIO.PktUp), `direction="transmit"`)
	writeMetric(&b, "phoenix_network_packets_total", float64(status.NetIO.PktDown), `direction="receive"`)
	writeMetricHelp(&b, "phoenix_process_up", "Whether a managed service is running")
	writeMetric(&b, "phoenix_process_up", processStateValue(status.Xray.State), `service="xray"`)
	writeMetric(&b, "phoenix_process_up", processStateValue(status.OpenVPN.State), `service="openvpn"`)
	writeMetric(&b, "phoenix_process_up", processStateValue(status.L2TP.State), `service="l2tp"`)
	writeMetric(&b, "phoenix_process_up", processStateValue(status.PPPVPN.State), `service="pppvpn"`)
	writeMetricHelp(&b, "phoenix_online_clients", "Online clients reported by managed daemon")
	writeMetric(&b, "phoenix_online_clients", float64(status.OpenVPN.OnlineClients), `service="openvpn"`)
	writeMetric(&b, "phoenix_online_clients", float64(status.L2TP.OnlineClients), `service="l2tp"`)
	writeMetric(&b, "phoenix_online_clients", float64(status.PPPVPN.OnlineClients), `service="pppvpn"`)
	writeMetricHelp(&b, "phoenix_connections", "Open host connections")
	writeMetric(&b, "phoenix_connections", float64(status.TcpCount), `protocol="tcp"`)
	writeMetric(&b, "phoenix_connections", float64(status.UdpCount), `protocol="udp"`)
	writeMetricHelp(&b, "phoenix_panel_info", "PHOENIX panel version information")
	writeMetric(&b, "phoenix_panel_info", 1, `version=`+strconv.Quote(status.PanelVersion))
	writeMetricHelp(&b, "phoenix_panel_uptime_seconds", "Panel process uptime in seconds")
	writeMetric(&b, "phoenix_panel_uptime_seconds", float64(status.AppStats.Uptime))
	writeMetricHelp(&b, "phoenix_host_uptime_seconds", "Host uptime in seconds")
	writeMetric(&b, "phoenix_host_uptime_seconds", float64(status.Uptime))
	writeMetricHelp(&b, "phoenix_catalog_entries", "Cached entries in enabled rule-set catalogs")
	var catalogEntries int
	if database.GetDB() != nil {
		if catalogs, err := (service.RuleSetCatalogService{}).List(false); err == nil {
			for _, catalog := range catalogs {
				if catalog.Enabled {
					catalogEntries += catalog.EntryCount
				}
			}
		}
	}
	writeMetric(&b, "phoenix_catalog_entries", float64(catalogEntries))
	writeMetricHelp(&b, "phoenix_scrape_timestamp_seconds", "Unix timestamp at which the metrics were rendered")
	writeMetric(&b, "phoenix_scrape_timestamp_seconds", float64(time.Now().Unix()))
	return b.String()
}

func processStateValue(state service.ProcessState) float64 {
	if state == service.Running {
		return 1
	}
	return 0
}

func writeMetricHelp(b *strings.Builder, name, help string) {
	writeMetricMeta(b, name, help, "gauge")
}

func writeMetricCounterHelp(b *strings.Builder, name, help string) {
	writeMetricMeta(b, name, help, "counter")
}

func writeMetricMeta(b *strings.Builder, name, help, metricType string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, metricType)
}

func writeMetric(b *strings.Builder, name string, value float64, labels ...string) {
	b.WriteString(name)
	if len(labels) > 0 {
		b.WriteByte('{')
		b.WriteString(strings.Join(labels, ","))
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
	b.WriteByte('\n')
}
