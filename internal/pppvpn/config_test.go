package pppvpn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPPTPDConfigOnlyEnablesLogwtmpWhenPluginExists(t *testing.T) {
	originalPath := pptpdLogwtmpPluginPath
	t.Cleanup(func() { pptpdLogwtmpPluginPath = originalPath })

	pluginPath := filepath.Join(t.TempDir(), "pptpd-logwtmp.so")
	pptpdLogwtmpPluginPath = pluginPath
	instance := Instance{
		ID:       1,
		Protocol: PPTP,
		LocalIP:  "10.253.0.1",
		PoolCIDR: "10.253.0.0/24",
	}

	config := renderPPTPDConfig(instance, "/etc/ppp/options.phoenix")
	if strings.Contains(config, "logwtmp") {
		t.Fatal("PPTP config enabled logwtmp without the plugin")
	}

	if err := os.WriteFile(pluginPath, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	config = renderPPTPDConfig(instance, "/etc/ppp/options.phoenix")
	if !strings.Contains(config, "\nlogwtmp\n") {
		t.Fatal("PPTP config did not enable logwtmp when the plugin exists")
	}
}
