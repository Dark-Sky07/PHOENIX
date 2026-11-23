package pppvpn

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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

var pptpdLogwtmpPluginPath = "/usr/lib/pptpd/pptpd-logwtmp.so"

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

func instanceRelativeDir(i Instance) (string, error) {
	protocolDir := ""
	switch i.Protocol {
	case SSTP:
		protocolDir = "sstp"
	case PPTP:
		protocolDir = "pptp"
	default:
		return "", fmt.Errorf("pppvpn: invalid protocol directory %q", i.Protocol)
	}
	if i.ID <= 0 {
		return "", errors.New("pppvpn: inbound id must be positive")
	}
	idDir := strconv.Itoa(i.ID)
	if !filepath.IsLocal(idDir) {
		return "", errors.New("pppvpn: invalid inbound directory")
	}
	relative := filepath.Join(protocolDir, idDir)
	if !filepath.IsLocal(relative) {
		return "", errors.New("pppvpn: invalid instance directory")
	}
	return relative, nil
}

func dirFor(i Instance) string {
	relative, err := instanceRelativeDir(i)
	if err != nil {
		return ""
	}
	return filepath.Join(rootDir(), relative)
}

func removeInstanceDir(i Instance) error {
	relative, err := instanceRelativeDir(i)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(rootDir())
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(relative)
}

func quotePPP(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func renderPPPDOptions(i Instance) string {
	server := pppServerName(i)
	ipUp := filepath.Join(dirFor(i), "ip-up")
	ipDown := filepath.Join(dirFor(i), "ip-down")
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
ip-down-script %s
`, server, i.DNS1, i.DNS2, ipUp, ipDown)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func renderTrafficHook(sessionDir string, inboundID int, closed bool) string {
	state := "active"
	if closed {
		state = "closed"
	}
	return fmt.Sprintf(`#!/bin/sh
set -eu
umask 077
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
iface="${IFNAME:-${1:-}}"
peer="${PEERNAME:-}"
case "$iface" in ''|.|..|*[!A-Za-z0-9_.-]*) exit 1 ;; esac
session_dir=%s
mkdir -p "$session_dir"
chmod 700 "$session_dir"
# Every unexpected failure after this point leaves a visible, deliberately
# unparseable incident for the panel collector to fence and an operator to review.
incident() {
  code=$?
  trap - EXIT
  if [ "$code" -ne 0 ]; then
    file=$(mktemp "$session_dir/hook-error.XXXXXXXXXX") || exit "$code"
    printf 'PPP hook failure: %%s interface %%s\n' '%s' "$iface" > "$file" || true
    sync "$file" "$session_dir" || true
  fi
  exit "$code"
}
trap incident EXIT
# Persist an in-progress record before any session mutation. An interrupted
# hook leaves it for manual review; a running hook may finish and remove it.
pending=$(mktemp "$session_dir/hook-pending.XXXXXXXXXX")
printf 'PPP hook in progress: %%s interface %%s\n' '%s' "$iface" > "$pending"
sync "$pending" "$session_dir"
mkdir -p "$session_dir/.hook-locks"
exec 9>"$session_dir/.hook-locks/$iface"
flock -x -w 5 9
active_file="$session_dir/$iface"
# PPPD_PID is supplied by pppd. A reused PID alone is not a session identity:
# require this hook to be its child and include boot ID and process start time.
pid="${PPPD_PID:-}"
case "$pid" in ''|*[!0-9]*) exit 1 ;; esac
[ "$pid" = "$PPID" ] || exit 1
boot=$(cat /proc/sys/kernel/random/boot_id)
start=$(sed 's/^.*) //' "/proc/$pid/stat" | awk '{print $20}')
case "$start" in ''|*[!0-9]*) exit 1 ;; esac
owner="$boot:$pid:$start"
ifindex=$(cat "/sys/class/net/$iface/ifindex")
case "$ifindex" in ''|*[!0-9]*) exit 1 ;; esac
rx=0
tx=0
if [ '%s' = closed ]; then
  [ -f "$active_file" ] || exit 1
  saved_peer=$(cut -f3 "$active_file")
  session_id=$(cut -f6 "$active_file")
  saved_owner=$(cut -f8 "$active_file")
  saved_ifindex=$(cut -f9 "$active_file")
  [ -n "$session_id" ] && [ "$saved_owner" = "$owner" ] && [ "$saved_ifindex" = "$ifindex" ] || exit 1
  [ -z "$peer" ] || [ "$peer" = "$saved_peer" ] || exit 1
  peer="$saved_peer"
  rx=$(cat "/sys/class/net/$iface/statistics/rx_bytes")
  tx=$(cat "/sys/class/net/$iface/statistics/tx_bytes")
  [ "$(cat "/sys/class/net/$iface/ifindex")" = "$ifindex" ] || exit 1
  session_file="$session_dir/$iface.closed.$session_id"
  # Existing closed evidence must not be overwritten.
  [ ! -e "$session_file" ] || exit 1
else
  [ -n "$peer" ] && [ ! -e "$active_file" ] || exit 1
  session_id=$(cat /proc/sys/kernel/random/uuid)
  session_file="$active_file"
fi
tmp="$session_file.tmp.$$"
printf '%%s\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\t%%s\n' '%s' "$iface" "$peer" "$rx" "$tx" "$session_id" '%d' "$owner" "$ifindex" > "$tmp"
chmod 600 "$tmp"
sync "$tmp"
# Link is atomic and refuses to replace an existing record.
ln "$tmp" "$session_file"
rm -f "$tmp"
if [ '%s' = closed ]; then rm "$active_file"; fi
sync "$session_dir"
rm "$pending"
sync "$session_dir"
trap - EXIT
`, shellQuote(sessionDir), state, state, state, state, inboundID, state)
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
	b.WriteString("stimeout 60\nconnections 100\n")
	if _, err := os.Stat(pptpdLogwtmpPluginPath); err == nil {
		b.WriteString("logwtmp\n")
	}
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
	return atomicWrite(filepath.Dir(chapSecretsPath), filepath.Base(chapSecretsPath), []byte(content), 0o600)
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
	_ = atomicWrite(filepath.Dir(chapSecretsPath), filepath.Base(chapSecretsPath), []byte(content), 0o600)
}

func atomicWrite(rootPath, relativePath string, data []byte, mode os.FileMode) error {
	if relativePath == "" || !filepath.IsLocal(relativePath) || filepath.Clean(relativePath) != relativePath {
		return errors.New("pppvpn: refusing non-local managed path")
	}
	if err := os.MkdirAll(rootPath, 0o750); err != nil {
		return err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer root.Close()

	dir := filepath.Dir(relativePath)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}

	for attempt := 0; attempt < 10; attempt++ {
		token := make([]byte, 16)
		if _, err := rand.Read(token); err != nil {
			return err
		}
		tempPath := filepath.Join(dir, ".phoenix-pppvpn-"+hex.EncodeToString(token))
		if dir == "." {
			tempPath = ".phoenix-pppvpn-" + hex.EncodeToString(token)
		}
		tmp, err := root.OpenFile(tempPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err = tmp.Chmod(mode); err == nil {
			_, err = tmp.Write(data)
		}
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			_ = root.Remove(tempPath)
			return err
		}
		if err = root.Rename(tempPath, relativePath); err != nil {
			_ = root.Remove(tempPath)
			return err
		}
		return nil
	}
	return errors.New("pppvpn: could not allocate a temporary config file")
}

func writeConfig(i Instance) (string, error) {
	if err := Validate(i); err != nil {
		return "", err
	}
	relativeDir, err := instanceRelativeDir(i)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(rootDir(), relativeDir)
	optionsRelative := filepath.Join(relativeDir, "options.ppp")
	options := filepath.Join(dir, "options.ppp")
	if err := atomicWrite(rootDir(), optionsRelative, []byte(renderPPPDOptions(i)), 0o600); err != nil {
		return "", err
	}
	// Keep accounting records outside the managed instance directory: Remove()
	// deletes daemon configs/certificates after stopping it, but any final PPP
	// counters still need to be drained by the next traffic poll.
	sessionDir := filepath.Join(rootDir(), "sessions", fmt.Sprintf("%s-%d", i.Protocol, i.ID))
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		return "", fmt.Errorf("prepare PPP session spool: %w", err)
	}
	for _, dir := range []string{sessionDir, filepath.Dir(sessionDir), rootDir()} {
		if err := syncPPPDirectory(dir); err != nil {
			return "", err
		}
	}
	ipUp := renderTrafficHook(sessionDir, i.ID, false) + "\nsysctl -w \"net.ipv4.conf.${IFNAME:-${1:-}}.route_localnet=1\" >/dev/null 2>&1 || true\nsysctl -w \"net.ipv4.conf.${IFNAME:-${1:-}}.rp_filter=0\" >/dev/null 2>&1 || true\n"
	if err := atomicWrite(rootDir(), filepath.Join(relativeDir, "ip-up"), []byte(ipUp), 0o750); err != nil {
		return "", err
	}
	if err := atomicWrite(rootDir(), filepath.Join(relativeDir, "ip-down"), []byte(renderTrafficHook(sessionDir, i.ID, true)), 0o750); err != nil {
		return "", err
	}
	if err := updateChapSecrets(i); err != nil {
		return "", fmt.Errorf("write PPP credentials: %w", err)
	}
	if i.Protocol == PPTP {
		relativePath := filepath.Join(relativeDir, "pptpd.conf")
		path := filepath.Join(rootDir(), relativePath)
		return path, atomicWrite(rootDir(), relativePath, []byte(renderPPTPDConfig(i, options)), 0o600)
	}
	certRelative, keyRelative := filepath.Join(relativeDir, "server.crt"), filepath.Join(relativeDir, "server.key")
	certPath, keyPath := filepath.Join(rootDir(), certRelative), filepath.Join(rootDir(), keyRelative)
	if err := atomicWrite(rootDir(), certRelative, []byte(i.CertificatePEM), 0o600); err != nil {
		return "", err
	}
	if err := atomicWrite(rootDir(), keyRelative, []byte(i.PrivateKeyPEM), 0o600); err != nil {
		return "", err
	}
	relativePath := filepath.Join(relativeDir, "sstpd.conf")
	path := filepath.Join(rootDir(), relativePath)
	// sstpd reads an INI section; the instance-specific files make parallel
	// server instances isolated and allow a dedicated certificate per inbound.
	cfg := fmt.Sprintf("[server]\nlisten = %s\nlisten_port = %d\npem_cert = %s\npem_key = %s\nlocal = %s\nremote = %s\npppd_config = %s\n", strings.TrimSpace(i.Listen), i.Port, certPath, keyPath, i.LocalIP, i.PoolCIDR, options)
	return path, atomicWrite(rootDir(), relativePath, []byte(cfg), 0o600)
}
