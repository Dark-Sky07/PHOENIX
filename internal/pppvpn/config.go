package pppvpn

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/config"
)

const chapSecretsPath = "/etc/ppp/chap-secrets"

func chapMarkers(i Instance) (string, string) {
	name := strings.ToUpper(string(i.Protocol)) + " " + strconv.Itoa(i.ID)
	return "# BEGIN PHOENIX " + name + " MANAGED CREDENTIALS", "# END PHOENIX " + name + " MANAGED CREDENTIALS"
}

func pppServerName(i Instance) string {
	return "phoenix-" + string(i.Protocol) + "-" + strconv.Itoa(i.ID)
}

var chapSecretsMu sync.Mutex

func rootDir() string {
	dir := config.GetBinFolderPath()
	if filepath.IsAbs(dir) {
		return filepath.Join(dir, "pppvpn")
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), dir, "pppvpn")
	}
	abs, err := filepath.Abs(dir)
	if err == nil {
		return filepath.Join(abs, "pppvpn")
	}
	return filepath.Join(dir, "pppvpn")
}

func dirFor(i Instance) string {
	return filepath.Join(rootDir(), string(i.Protocol), strconv.Itoa(i.ID))
}

func quotePPP(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func renderPPPDOptions(i Instance) string {
	server := pppServerName(i)
	ipUp := filepath.Join(dirFor(i), "ip-up")
	return fmt.Sprintf(`# Managed by PHOENIX. Do not edit.
name %s
auth
require-mschap-v2
refuse-pap
refuse-chap
refuse-mschap
require-mppe-128
proxyarp
noipdefault
nodefaultroute
lock
mtu 1400
mru 1400
ms-dns %s
ms-dns %s
ip-up-script %s
`, server, i.DNS1, i.DNS2, ipUp)
}

func renderPPTPDConfig(i Instance, optionsPath string) string {
	_, network, _ := net.ParseCIDR(i.PoolCIDR)
	start := append(net.IP(nil), network.IP.To4()...)
	end := append(net.IP(nil), network.IP.To4()...)
	start[3] += 2
	end[3] = 254
	listen := strings.TrimSpace(i.Listen)
	if listen == "" || listen == "0.0.0.0" {
		listen = ""
	}
	var b strings.Builder
	b.WriteString("# Managed by PHOENIX. Do not edit.\n")
	if listen != "" {
		fmt.Fprintf(&b, "listen %s\n", listen)
	}
	fmt.Fprintf(&b, "option %s\nlocalip %s\nremoteip %s-%s\n", optionsPath, i.LocalIP, start, end)
	b.WriteString("stimeout 60\nconnections 100\nlogwtmp\n")
	return b.String()
}

func renderChapBlock(i Instance) string {
	server := pppServerName(i)
	begin, end := chapMarkers(i)
	var b strings.Builder
	b.WriteString(begin + "\n")
	for _, c := range i.Credentials {
		fmt.Fprintf(&b, "%s %s %s *\n", quotePPP(c.Username), server, quotePPP(c.Password))
	}
	b.WriteString(end + "\n")
	return b.String()
}

func updateChapSecrets(i Instance) error {
	chapSecretsMu.Lock()
	defer chapSecretsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(chapSecretsPath), 0o750); err != nil {
		return err
	}
	old, err := os.ReadFile(chapSecretsPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	begin, end := chapMarkers(i)
	lines := strings.Split(string(old), "\n")
	kept := make([]string, 0, len(lines))
	inside := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == begin {
			inside = true
			continue
		}
		if trim == end {
			inside = false
			continue
		}
		if !inside {
			kept = append(kept, line)
		}
	}
	base := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	block := strings.TrimRight(renderChapBlock(i), "\n")
	content := block + "\n"
	if base != "" {
		content = base + "\n\n" + content
	}
	return atomicWrite(chapSecretsPath, []byte(content), 0o600)
}

func removeChapSecrets(i Instance) {
	chapSecretsMu.Lock()
	defer chapSecretsMu.Unlock()
	old, err := os.ReadFile(chapSecretsPath)
	if err != nil {
		return
	}
	begin, end := chapMarkers(i)
	lines := strings.Split(string(old), "\n")
	kept := make([]string, 0, len(lines))
	inside := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == begin {
			inside = true
			continue
		}
		if trim == end {
			inside = false
			continue
		}
		if !inside {
			kept = append(kept, line)
		}
	}
	content := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if content != "" {
		content += "\n"
	}
	_ = atomicWrite(chapSecretsPath, []byte(content), 0o600)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".phoenix-pppvpn-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func writeConfig(i Instance) (string, error) {
	if err := Validate(i); err != nil {
		return "", err
	}
	dir := dirFor(i)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	options := filepath.Join(dir, "options.ppp")
	if err := atomicWrite(options, []byte(renderPPPDOptions(i)), 0o600); err != nil {
		return "", err
	}
	ipUp := "#!/bin/sh\nset -eu\nPATH=/usr/sbin:/usr/bin:/sbin:/bin\nexport PATH\niface=\"${IFNAME:-${1:-}}\"\ncase \"$iface\" in ''|*[!A-Za-z0-9_.-]*) exit 0 ;; esac\nsysctl -w \"net.ipv4.conf.$iface.route_localnet=1\" >/dev/null 2>&1 || true\nsysctl -w \"net.ipv4.conf.$iface.rp_filter=0\" >/dev/null 2>&1 || true\n"
	if err := atomicWrite(filepath.Join(dir, "ip-up"), []byte(ipUp), 0o750); err != nil {
		return "", err
	}
	if err := updateChapSecrets(i); err != nil {
		return "", fmt.Errorf("write PPP credentials: %w", err)
	}
	if i.Protocol == PPTP {
		path := filepath.Join(dir, "pptpd.conf")
		return path, atomicWrite(path, []byte(renderPPTPDConfig(i, options)), 0o600)
	}
	certPath, keyPath := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := atomicWrite(certPath, []byte(i.CertificatePEM), 0o600); err != nil {
		return "", err
	}
	if err := atomicWrite(keyPath, []byte(i.PrivateKeyPEM), 0o600); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "sstpd.conf")
	// sstpd reads an INI section; the instance-specific files make parallel
	// server instances isolated and allow a dedicated certificate per inbound.
	cfg := fmt.Sprintf("[server]\nlisten = %s\nlisten_port = %d\npem_cert = %s\npem_key = %s\nlocal = %s\nremote = %s\npppd_config = %s\n", strings.TrimSpace(i.Listen), i.Port, certPath, keyPath, i.LocalIP, i.PoolCIDR, options)
	return path, atomicWrite(path, []byte(cfg), 0o600)
}
