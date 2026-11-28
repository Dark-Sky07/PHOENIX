package fleetagent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

// tunnelManager supervises only known, locally configured connector adapters.
// The control plane selects a tunnel contract but never selects an executable,
// arguments, or an environment variable. This separation prevents a template
// editor from becoming remote code execution on a node.
type tunnelManager struct {
	mu              sync.Mutex
	dir             string
	cloudflaredPath string
	processes       map[int]*tunnelProcess
	desired         map[int]struct{}
	reports         map[int]service.AgentTunnelReport
	closed          bool
}

type tunnelProcess struct {
	hash    string
	cmd     *exec.Cmd
	stopped bool
}

type cloudflaredProviderConfig struct {
	TunnelID    string          `json:"tunnelId"`
	Credentials json.RawMessage `json:"credentials"`
}

const connectorStopGracePeriod = 10 * time.Second

var tunnelUUIDRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{12}$`)

func newTunnelManager(dir, cloudflaredPath string) (*tunnelManager, error) {
	if dir == "" {
		return nil, errors.New("managed tunnel directory is required")
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if cloudflaredPath != "" {
		cloudflaredPath, err = validateConnectorBinary(cloudflaredPath)
		if err != nil {
			return nil, fmt.Errorf("invalid cloudflared path: %w", err)
		}
	}
	if err := os.MkdirAll(absoluteDir, 0o700); err != nil {
		return nil, fmt.Errorf("create managed tunnel directory: %w", err)
	}
	if err := os.Chmod(absoluteDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect managed tunnel directory: %w", err)
	}
	return &tunnelManager{
		dir:             absoluteDir,
		cloudflaredPath: cloudflaredPath,
		processes:       make(map[int]*tunnelProcess),
		desired:         make(map[int]struct{}),
		reports:         make(map[int]service.AgentTunnelReport),
	}, nil
}

func (m *tunnelManager) SupportsCloudflared() bool { return m != nil && m.cloudflaredPath != "" }

// Reconcile converges process state to the delivered tunnel contracts. It is
// safe to call every heartbeat: unchanged running processes are preserved;
// changed contracts are stopped and recreated using newly written 0600 files.
func (m *tunnelManager) Reconcile(tunnels []service.AgentTunnelDelivery) error {
	if m == nil {
		return errors.New("managed tunnel supervisor is unavailable")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("managed tunnel supervisor is closed")
	}
	if len(tunnels) > 64 {
		return errors.New("too many managed tunnels")
	}

	next := make(map[int]struct{}, len(tunnels))
	var failures []string
	for _, tunnel := range tunnels {
		next[tunnel.ID] = struct{}{}
		hash, err := tunnelDeliveryHash(tunnel)
		if err != nil {
			failures = append(failures, fmt.Sprintf("tunnel %d: %v", tunnel.ID, err))
			m.setReportLocked(tunnel, "error", 0, err.Error())
			continue
		}
		if current := m.processes[tunnel.ID]; current != nil && current.cmd != nil && current.cmd.Process != nil {
			if current.hash == hash && !current.stopped {
				continue
			}
			// Do not overlap two processes for one tunnel. The exit watcher removes
			// this entry; the next heartbeat starts the new contract.
			m.stopLocked(tunnel.ID)
			m.setReportLocked(tunnel, "stopping", 0, "")
			failures = append(failures, fmt.Sprintf("tunnel %d: waiting for existing connector to stop", tunnel.ID))
			continue
		}
		if err := m.startLocked(tunnel, hash); err != nil {
			failures = append(failures, fmt.Sprintf("tunnel %d: %v", tunnel.ID, err))
			m.setReportLocked(tunnel, tunnelFailureStatus(err), 0, err.Error())
		}
	}
	for id := range m.desired {
		if _, keep := next[id]; keep {
			continue
		}
		m.stopLocked(id)
		delete(m.processes, id)
		m.removeTunnelFilesLocked(id)
		delete(m.reports, id)
	}
	m.desired = next
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func (m *tunnelManager) startLocked(tunnel service.AgentTunnelDelivery, hash string) error {
	if tunnel.Provider != "cloudflared" {
		return fmt.Errorf("provider %q has no installed agent adapter", tunnel.Provider)
	}
	if m.cloudflaredPath == "" {
		return errors.New("cloudflared adapter requires --cloudflared-path to an absolute executable")
	}
	configPath, err := m.writeCloudflaredConfigLocked(tunnel)
	if err != nil {
		return err
	}
	cmd := exec.Command(m.cloudflaredPath, "--no-autoupdate", "--config", configPath, "tunnel", "run")
	// Connector output can contain provider-specific details; do not copy it to
	// the panel or agent state. Bounded status below is sufficient for control.
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start cloudflared: %w", err)
	}
	process := &tunnelProcess{hash: hash, cmd: cmd}
	m.processes[tunnel.ID] = process
	m.setReportLocked(tunnel, "running", cmd.Process.Pid, "")
	go m.waitForExit(tunnel, process)
	return nil
}

func (m *tunnelManager) waitForExit(tunnel service.AgentTunnelDelivery, process *tunnelProcess) {
	err := process.cmd.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.processes[tunnel.ID] != process {
		return
	}
	if process.stopped || m.closed {
		m.setReportLocked(tunnel, "stopped", 0, "")
		delete(m.processes, tunnel.ID)
		return
	}
	process.stopped = true
	message := "connector exited"
	if err != nil {
		message = trimError(fmt.Sprintf("connector exited: %v", err))
	}
	m.setReportLocked(tunnel, "error", 0, message)
	delete(m.processes, tunnel.ID)
}

func (m *tunnelManager) writeCloudflaredConfigLocked(tunnel service.AgentTunnelDelivery) (string, error) {
	if tunnel.Protocol == "udp" {
		return "", errors.New("cloudflared does not support UDP ingress")
	}
	provider, err := parseCloudflaredProviderConfig(tunnel.ProviderConfig)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(m.dir, fmt.Sprintf("tunnel-%d", tunnel.ID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	credentialsPath := filepath.Join(dir, "credentials.json")
	if err := writePrivateFile(credentialsPath, provider.Credentials); err != nil {
		return "", fmt.Errorf("write cloudflared credentials: %w", err)
	}
	serviceURL := tunnel.Protocol + "://" + net.JoinHostPort(tunnel.TargetHost, strconv.Itoa(tunnel.TargetPort))
	config := strings.Join([]string{
		"tunnel: " + provider.TunnelID,
		"credentials-file: " + yamlSingleQuote(credentialsPath),
		"ingress:",
		"  - hostname: " + yamlSingleQuote(tunnel.PublicHostname),
		"    service: " + yamlSingleQuote(serviceURL),
		"  - service: http_status:404",
		"",
	}, "\n")
	configPath := filepath.Join(dir, "config.yml")
	if err := writePrivateFile(configPath, []byte(config)); err != nil {
		return "", fmt.Errorf("write cloudflared config: %w", err)
	}
	return configPath, nil
}

func parseCloudflaredProviderConfig(raw json.RawMessage) (cloudflaredProviderConfig, error) {
	if len(raw) == 0 || len(raw) > 16*1024 {
		return cloudflaredProviderConfig{}, errors.New("cloudflared providerConfig is required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var config cloudflaredProviderConfig
	if err := decoder.Decode(&config); err != nil {
		return cloudflaredProviderConfig{}, errors.New("invalid cloudflared providerConfig")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return cloudflaredProviderConfig{}, errors.New("invalid cloudflared providerConfig")
	}
	if !tunnelUUIDRE.MatchString(config.TunnelID) || len(config.Credentials) == 0 || !json.Valid(config.Credentials) {
		return cloudflaredProviderConfig{}, errors.New("cloudflared config requires tunnelId and credentials")
	}
	var credentialFields struct {
		AccountTag   string `json:"AccountTag"`
		TunnelSecret string `json:"TunnelSecret"`
		TunnelID     string `json:"TunnelID"`
	}
	if err := json.Unmarshal(config.Credentials, &credentialFields); err != nil || credentialFields.AccountTag == "" || credentialFields.TunnelSecret == "" || !strings.EqualFold(credentialFields.TunnelID, config.TunnelID) {
		return cloudflaredProviderConfig{}, errors.New("invalid cloudflared credentials")
	}
	return config, nil
}

func (m *tunnelManager) setReportLocked(tunnel service.AgentTunnelDelivery, status string, pid int, lastError string) {
	m.reports[tunnel.ID] = service.AgentTunnelReport{
		TunnelID: tunnel.ID, Provider: tunnel.Provider, Status: status, PID: pid,
		LastError: trimError(lastError), ObservedAt: time.Now().UnixMilli(),
	}
}

func (m *tunnelManager) stopLocked(id int) {
	process := m.processes[id]
	if process == nil || process.cmd == nil || process.cmd.Process == nil || process.stopped {
		return
	}
	process.stopped = true
	// No shell is involved, so a remote document cannot affect what is
	// signalled. A bounded hard-stop fallback prevents a connector that ignores
	// INT from surviving a withdrawal or blocking a later contract replacement.
	if err := process.cmd.Process.Signal(os.Interrupt); err != nil {
		_ = process.cmd.Process.Kill()
		return
	}
	time.AfterFunc(connectorStopGracePeriod, func() {
		_ = process.cmd.Process.Kill()
	})
}

func (m *tunnelManager) StopAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make(map[int]struct{}, len(m.desired)+len(m.processes))
	for id := range m.desired {
		ids[id] = struct{}{}
	}
	for id := range m.processes {
		ids[id] = struct{}{}
	}
	for id := range ids {
		m.stopLocked(id)
		delete(m.processes, id)
		m.removeTunnelFilesLocked(id)
	}
	m.desired = make(map[int]struct{})
	m.reports = make(map[int]service.AgentTunnelReport)
}

func (m *tunnelManager) removeTunnelFilesLocked(id int) {
	if id <= 0 {
		return
	}
	// The directory name is constructed from a validated positive database ID,
	// never from a panel-supplied path. Removal therefore cannot escape dir.
	_ = os.RemoveAll(filepath.Join(m.dir, fmt.Sprintf("tunnel-%d", id)))
}

func (m *tunnelManager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	for id := range m.processes {
		m.stopLocked(id)
	}
	m.mu.Unlock()
}

func (m *tunnelManager) Reports() []service.AgentTunnelReport {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	reports := make([]service.AgentTunnelReport, 0, len(m.desired))
	for id := range m.desired {
		if report, ok := m.reports[id]; ok {
			reports = append(reports, report)
		}
	}
	// Stable ordering makes check-ins deterministic and easier to audit.
	for i := 0; i < len(reports); i++ {
		for j := i + 1; j < len(reports); j++ {
			if reports[j].TunnelID < reports[i].TunnelID {
				reports[i], reports[j] = reports[j], reports[i]
			}
		}
	}
	return reports
}

func tunnelDeliveryHash(tunnel service.AgentTunnelDelivery) (string, error) {
	encoded, err := json.Marshal(tunnel)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func validateConnectorBinary(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("connector executable path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("connector path is not an executable regular file")
	}
	return filepath.Clean(path), nil
}

func writePrivateFile(path string, body []byte) error {
	if len(body) > 64*1024 {
		return errors.New("managed tunnel file is too large")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".phoenix-tunnel-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func yamlSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func tunnelFailureStatus(err error) string {
	if err != nil && (strings.Contains(err.Error(), "no installed agent adapter") || strings.Contains(err.Error(), "requires --cloudflared-path")) {
		return "unsupported"
	}
	return "error"
}

func trimError(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		return value[:512]
	}
	return value
}
