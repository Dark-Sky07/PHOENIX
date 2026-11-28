// Package fleetagent implements PHOENIX's outbound-only fleet agent. It has no
// listener and accepts no commands: it polls the control plane, validates the
// narrow desired-state document, and persists it with owner-only permissions.
package fleetagent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

const (
	defaultPollInterval      = 15 * time.Second
	maxAgentResponse         = 128 * 1024
	maxOfficialUpdateArchive = 200 * 1024 * 1024
)

// ErrRestartRequired is returned only after a verified official binary has
// atomically replaced the locally configured agent binary. A service manager
// should restart the agent so it can report the completed update.
var ErrRestartRequired = errors.New("fleet agent restart required after update")

var officialReleaseTagRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+-phoenix$`)

type Config struct {
	PanelURL           string
	BootstrapToken     string
	StatePath          string
	TunnelDir          string
	CloudflaredPath    string
	PollInterval       time.Duration
	CertSHA256         string
	AllowHTTP          bool
	Once               bool
	UpdateBinaryPath   string
	UpdateArchitecture string
}

type State struct {
	AgentID         string                     `json:"agentId"`
	Credential      string                     `json:"credential"`
	DesiredState    *service.AgentDesiredState `json:"desiredState,omitempty"`
	DesiredActive   bool                       `json:"desiredActive"`
	AppliedRevision int                        `json:"appliedRevision"`
	ApplyError      string                     `json:"applyError,omitempty"`
	Update          *updateState               `json:"update,omitempty"`
	UpdatedAt       int64                      `json:"updatedAt"`
}

type updateState struct {
	RequestedAt int64  `json:"requestedAt"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type Agent struct {
	config     Config
	client     *http.Client
	state      State
	tunnels    *tunnelManager
	updateArch string
}

func New(config Config) (*Agent, error) {
	if config.StatePath == "" {
		return nil, errors.New("agent state path is required")
	}
	if config.PollInterval == 0 {
		config.PollInterval = defaultPollInterval
	}
	if config.PollInterval < 5*time.Second || config.PollInterval > 5*time.Minute {
		return nil, errors.New("agent poll interval must be between 5 seconds and 5 minutes")
	}
	if config.TunnelDir == "" {
		config.TunnelDir = filepath.Join(filepath.Dir(config.StatePath), "managed-tunnels")
	}
	if _, err := parsePanelURL(config.PanelURL, config.AllowHTTP); err != nil {
		return nil, err
	}
	updateArch := ""
	if config.UpdateBinaryPath != "" {
		if config.UpdateArchitecture != "" {
			if !validOfficialArchitecture(config.UpdateArchitecture) {
				return nil, errors.New("official update architecture is invalid")
			}
			updateArch = config.UpdateArchitecture
		} else {
			var err error
			updateArch, err = officialUpdateArchitecture()
			if err != nil {
				return nil, err
			}
		}
	}
	if err := validateOfficialUpdateConfig(config.UpdateBinaryPath); err != nil {
		return nil, err
	}
	tlsConfig, err := tlsConfigForPin(config.CertSHA256)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: transport,
		// Never follow a redirect with the agent credential. A panel deployment
		// needing a different base URL must be configured explicitly instead.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	tunnels, err := newTunnelManager(config.TunnelDir, config.CloudflaredPath)
	if err != nil {
		return nil, err
	}
	a := &Agent{config: config, client: client, tunnels: tunnels, updateArch: updateArch}
	if err := a.loadState(); err != nil {
		return nil, err
	}
	if a.state.AgentID == "" {
		id, err := randomAgentID()
		if err != nil {
			return nil, err
		}
		a.state.AgentID = id
	}
	return a, nil
}

func (a *Agent) Run(ctx context.Context) error {
	defer a.tunnels.Close()
	if err := a.SyncOnce(ctx); err != nil {
		return err
	}
	if a.config.Once {
		return nil
	}
	ticker := time.NewTicker(a.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := a.SyncOnce(ctx); err != nil {
				if errors.Is(err, ErrRestartRequired) {
					return err
				}
				// The caller owns logging/restart policy. Keeping the loop alive
				// lets a temporary panel outage preserve the last staged state.
				continue
			}
		}
	}
}

// SyncOnce is exported for deterministic tests and a systemd-style supervisor.
func (a *Agent) SyncOnce(ctx context.Context) error {
	if a.state.Credential == "" {
		if a.config.BootstrapToken == "" {
			return errors.New("agent is not enrolled and no bootstrap token was supplied")
		}
		return a.enroll(ctx)
	}

	// A restarted agent must re-establish any previously acknowledged managed
	// tunnel before it tells the panel that revision is still healthy.
	if a.state.DesiredActive && a.state.DesiredState != nil {
		if err := a.applyDesiredState(a.state.DesiredState); err != nil {
			a.state.ApplyError = truncateError(err)
		} else {
			a.state.ApplyError = ""
			a.state.AppliedRevision = a.state.DesiredState.Revision
		}
	}
	request := service.AgentCheckInRequest{
		AgentID:         a.state.AgentID,
		Version:         agentVersion(),
		Capabilities:    a.capabilities(),
		AppliedRevision: a.state.AppliedRevision,
		ApplyError:      a.state.ApplyError,
		Tunnels:         a.tunnels.Reports(),
	}
	if a.state.Update != nil {
		request.Update = &service.AgentUpdateReport{RequestedAt: a.state.Update.RequestedAt, Status: a.state.Update.Status, Error: a.state.Update.Error}
	}
	var response service.AgentCheckInResponse
	if err := a.post(ctx, "/panel/agent/v1/checkin", a.state.Credential, request, &response); err != nil {
		return err
	}
	if response.Disabled {
		// Disabled enrollments get one safe terminal response rather than an
		// arbitrary remote command. Stop connectors and persist inactive state;
		// a later re-enable can deliver a normal desired state on the next poll.
		a.tunnels.StopAll()
		a.state.DesiredActive = false
		a.state.ApplyError = ""
	} else {
		if response.Credential != "" {
			a.state.Credential = response.Credential
			a.state.UpdatedAt = time.Now().UnixMilli()
			// Persist a replacement before it can be used on a later poll. This
			// avoids losing a rotated credential if connector reconciliation fails.
			if err := a.saveState(); err != nil {
				return err
			}
		}
		if response.DesiredState != nil && (!a.state.DesiredActive || !desiredStateMatches(a.state.DesiredState, response.DesiredState) || a.state.ApplyError != "") {
			if err := a.applyDesiredState(response.DesiredState); err != nil {
				// Persist the desired contract despite a failed reconciliation so a
				// later heartbeat can retry it. AppliedRevision intentionally stays
				// unchanged until every managed connector converges.
				a.state.DesiredState = response.DesiredState
				a.state.DesiredActive = true
				a.state.ApplyError = truncateError(err)
			} else {
				a.state.DesiredState = response.DesiredState
				a.state.DesiredActive = true
				a.state.AppliedRevision = response.DesiredState.Revision
				a.state.ApplyError = ""
			}
		} else if response.DesiredState != nil {
			// A previously applied contract remains active and does not need to be
			// restarted after every response; it was reconciled before this request.
			a.state.DesiredActive = true
		} else {
			// No assignment or a disabled template withdraws the desired endpoint.
			a.tunnels.StopAll()
			a.state.DesiredActive = false
			a.state.ApplyError = ""
		}
	}
	a.state.UpdatedAt = time.Now().UnixMilli()
	if err := a.saveState(); err != nil {
		return err
	}
	if response.Update != nil && !response.Disabled {
		return a.applyOfficialUpdate(ctx, response.Update)
	}
	return nil
}

func (a *Agent) applyOfficialUpdate(ctx context.Context, instruction *service.AgentUpdateInstruction) error {
	if instruction == nil {
		return nil
	}
	if !validOfficialUpdateInstruction(instruction, a.updateArch) || a.config.UpdateBinaryPath == "" {
		a.state.Update = &updateState{RequestedAt: instruction.RequestedAt, Status: "failed", Error: "official updater rejected the update instruction"}
		a.state.UpdatedAt = time.Now().UnixMilli()
		return a.saveState()
	}
	if a.state.Update != nil && a.state.Update.RequestedAt == instruction.RequestedAt && a.state.Update.Status == "installed" {
		return nil
	}
	a.state.Update = &updateState{RequestedAt: instruction.RequestedAt, Status: "accepted"}
	a.state.UpdatedAt = time.Now().UnixMilli()
	if err := a.saveState(); err != nil {
		return err
	}
	if err := updateFromOfficialRelease(ctx, a.config.UpdateBinaryPath, instruction.ReleaseTag, a.updateArch, instruction.ArchiveSHA256); err != nil {
		a.state.Update = &updateState{RequestedAt: instruction.RequestedAt, Status: "failed", Error: truncateError(err)}
		a.state.UpdatedAt = time.Now().UnixMilli()
		return a.saveState()
	}
	a.state.Update = &updateState{RequestedAt: instruction.RequestedAt, Status: "installed"}
	a.state.UpdatedAt = time.Now().UnixMilli()
	if err := a.saveState(); err != nil {
		return err
	}
	return ErrRestartRequired
}

func validateOfficialUpdateConfig(binaryPath string) error {
	if binaryPath == "" {
		return nil
	}
	if binaryPath != "/usr/local/bin/x-ui" {
		return errors.New("official updates are limited to /usr/local/bin/x-ui")
	}
	return nil
}

func officialUpdateArchitecture() (string, error) {
	switch runtime.GOARCH {
	case "amd64", "arm64", "386", "s390x":
		return runtime.GOARCH, nil
	case "arm":
		// Go does not expose ARM variants at runtime. A reviewed local installer
		// supplies its exact release architecture; guessing v5/v6/v7 here could
		// replace a working node with an incompatible official binary.
		return "", errors.New("official ARM updates require a locally pinned update architecture")
	default:
		return "", fmt.Errorf("official PHOENIX update is unsupported on %s", runtime.GOARCH)
	}
}

func validOfficialUpdateInstruction(instruction *service.AgentUpdateInstruction, localArchitecture string) bool {
	if instruction == nil || instruction.Source != "official-release-v1" || instruction.RequestedAt <= 0 || instruction.Architecture != localArchitecture || !officialReleaseTagRE.MatchString(instruction.ReleaseTag) {
		return false
	}
	digest, err := hex.DecodeString(instruction.ArchiveSHA256)
	return err == nil && len(digest) == sha256.Size
}

func validOfficialArchitecture(arch string) bool {
	switch arch {
	case "amd64", "arm64", "armv7", "armv6", "armv5", "386", "s390x":
		return true
	default:
		return false
	}
}

func updateFromOfficialRelease(ctx context.Context, binaryPath, tag, arch, expectedSHA256 string) error {
	if !officialReleaseTagRE.MatchString(tag) || !validOfficialArchitecture(arch) {
		return errors.New("official update metadata is invalid")
	}
	expected, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(expected) != sha256.Size {
		return errors.New("official update digest is invalid")
	}
	const repo = "Dark-Sky07/PHOENIX"
	archive := "x-ui-linux-" + arch + ".tar.gz"
	base := "https://github.com/" + repo + "/releases/download/" + tag + "/"
	checksum, err := downloadLimited(ctx, base+archive+".sha256", 4096)
	if err != nil {
		return err
	}
	sidecarDigest, err := checksumForArchive(checksum, archive)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(expected, sidecarDigest) != 1 {
		return errors.New("official archive checksum does not match the panel-pinned release")
	}
	payload, err := downloadLimited(ctx, base+archive, maxOfficialUpdateArchive)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(payload)
	if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
		return errors.New("official archive checksum verification failed")
	}
	binary, err := phoenixBinaryFromArchive(payload)
	if err != nil {
		return err
	}
	return atomicInstallBinary(binaryPath, binary)
}

func downloadLimited(ctx context.Context, rawURL string, maximum int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download official release asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official release asset returned %s", resp.Status)
	}
	if resp.ContentLength > maximum {
		return nil, errors.New("official release asset is too large")
	}
	reader := io.LimitReader(resp.Body, maximum+1)
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("official release asset is too large")
	}
	return data, nil
}

func checksumForArchive(sidecar []byte, archive string) ([]byte, error) {
	for _, line := range strings.Split(string(sidecar), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == archive && len(fields[0]) == sha256.Size*2 {
			decoded, err := hex.DecodeString(fields[0])
			if err == nil && len(decoded) == sha256.Size {
				return decoded, nil
			}
		}
	}
	return nil, errors.New("official archive checksum sidecar is invalid")
}

func phoenixBinaryFromArchive(payload []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("official archive is not gzip")
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read official archive: %w", err)
		}
		if header.Name != "x-ui/x-ui" {
			continue
		}
		if !header.FileInfo().Mode().IsRegular() || header.Size <= 0 || header.Size > maxOfficialUpdateArchive {
			return nil, errors.New("official archive binary entry is invalid")
		}
		binary, err := io.ReadAll(io.LimitReader(tarReader, header.Size+1))
		if err != nil || int64(len(binary)) != header.Size {
			return nil, errors.New("official archive binary entry is truncated")
		}
		return binary, nil
	}
	return nil, errors.New("official archive has an unexpected layout")
}

func atomicInstallBinary(binaryPath string, binary []byte) error {
	info, err := os.Lstat(binaryPath)
	if err != nil {
		return fmt.Errorf("stat configured update binary: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return errors.New("configured update binary is not a protected regular file")
	}
	dir := filepath.Dir(binaryPath)
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat update binary directory: %w", err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("update binary directory is writable by group or others")
	}
	tmp, err := os.CreateTemp(dir, ".phoenix-update-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(binary); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, binaryPath)
}

func (a *Agent) enroll(ctx context.Context) error {
	request := service.AgentEnrollRequest{
		BootstrapToken: a.config.BootstrapToken,
		AgentID:        a.state.AgentID,
		Version:        agentVersion(),
		Capabilities:   a.capabilities(),
	}
	var response service.AgentEnrollResponse
	if err := a.post(ctx, "/panel/agent/v1/enroll", "", request, &response); err != nil {
		return err
	}
	if len(response.Credential) < 32 {
		return errors.New("panel returned invalid agent credential")
	}
	a.state.Credential = response.Credential
	// The one-time bootstrap value has been consumed; do not retain it in the
	// long-lived agent object after a credential is minted.
	a.config.BootstrapToken = ""
	a.state.UpdatedAt = time.Now().UnixMilli()
	if err := a.saveState(); err != nil {
		return err
	}
	if response.DesiredState != nil {
		if err := a.applyDesiredState(response.DesiredState); err != nil {
			// Keep the contract for retry, but never acknowledge its revision
			// until the supervisor has reconciled every supported connector.
			a.state.DesiredState = response.DesiredState
			a.state.DesiredActive = true
			a.state.ApplyError = truncateError(err)
		} else {
			a.state.DesiredState = response.DesiredState
			a.state.DesiredActive = true
			a.state.AppliedRevision = response.DesiredState.Revision
			a.state.ApplyError = ""
		}
	}
	a.state.UpdatedAt = time.Now().UnixMilli()
	return a.saveState()
}

func agentVersion() string {
	return "phoenix-agent/" + config.GetVersion()
}

func (a *Agent) capabilities() json.RawMessage {
	managed := "contract-v1"
	if a.tunnels.SupportsCloudflared() {
		managed = "cloudflared-v1"
	}
	updates := "none"
	updateArch := ""
	if a.config.UpdateBinaryPath != "" {
		updates = "official-v1"
		updateArch = a.updateArch
	}
	return json.RawMessage(fmt.Sprintf(`{"desiredState":"apply-v1","managedTunnels":%q,"updates":%q,"updateArch":%q}`, managed, updates, updateArch))
}

func (a *Agent) applyDesiredState(state *service.AgentDesiredState) error {
	if err := validateDesiredState(state); err != nil {
		return err
	}
	return a.tunnels.Reconcile(state.Tunnels)
}

func (a *Agent) post(ctx context.Context, endpoint, credential string, input, output any) error {
	target, err := a.endpoint(endpoint)
	if err != nil {
		return err
	}
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if credential != "" {
		req.Header.Set("Authorization", "Bearer "+credential)
	}
	response, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("agent control-plane request failed: %w", err)
	}
	defer response.Body.Close()
	body, err = io.ReadAll(io.LimitReader(response.Body, maxAgentResponse+1))
	if err != nil {
		return err
	}
	if len(body) > maxAgentResponse {
		return errors.New("agent control-plane response too large")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("agent control-plane rejected request: HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(body, output); err != nil {
		return errors.New("invalid agent control-plane response")
	}
	return nil
}

func (a *Agent) endpoint(suffix string) (string, error) {
	base, err := parsePanelURL(a.config.PanelURL, a.config.AllowHTTP)
	if err != nil {
		return "", err
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + suffix
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

func (a *Agent) loadState() error {
	info, err := os.Stat(a.config.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("agent state file %s must not be readable by group or others", a.config.StatePath)
	}
	body, err := os.ReadFile(a.config.StatePath)
	if err != nil {
		return err
	}
	if len(body) > 256*1024 {
		return errors.New("agent state file is too large")
	}
	if err := json.Unmarshal(body, &a.state); err != nil {
		return errors.New("invalid agent state file")
	}
	if a.state.AgentID != "" && !validAgentID(a.state.AgentID) {
		return errors.New("invalid agent id in state file")
	}
	return nil
}

func (a *Agent) saveState() error {
	if err := os.MkdirAll(filepath.Dir(a.config.StatePath), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(a.config.StatePath), ".phoenix-agent-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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
	if err := os.Rename(tmpName, a.config.StatePath); err != nil {
		return err
	}
	return os.Chmod(a.config.StatePath, 0o600)
}

func parsePanelURL(raw string, allowHTTP bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Host == "" || u.User != nil {
		return nil, errors.New("agent panel URL must be an absolute URL without userinfo")
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return nil, errors.New("agent panel URL must use https (use --allow-http only for a trusted development network)")
	}
	return u, nil
}

func tlsConfigForPin(pin string) (*tls.Config, error) {
	pin = strings.TrimSpace(strings.ReplaceAll(pin, ":", ""))
	if pin == "" {
		return &tls.Config{MinVersion: tls.VersionTLS12}, nil
	}
	var want []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		want, err = encoding.DecodeString(pin)
		if err == nil && len(want) == sha256.Size {
			break
		}
		want = nil
	}
	if len(want) != sha256.Size {
		return nil, errors.New("agent certificate pin must be a base64 SHA-256 hash")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("panel did not present a TLS certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(sum[:], want) != 1 {
				return errors.New("panel certificate does not match configured SHA-256 pin")
			}
			return nil
		},
	}, nil
}

func desiredStateMatches(left, right *service.AgentDesiredState) bool {
	if left == nil || right == nil {
		return left == right
	}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func validateDesiredState(state *service.AgentDesiredState) error {
	if state == nil || state.TemplateID <= 0 || state.Revision <= 0 || state.Document.SchemaVersion != 1 {
		return errors.New("invalid desired fleet state")
	}
	if state.Document.Node.InboundSyncMode != "all" && state.Document.Node.InboundSyncMode != "selected" {
		return errors.New("invalid desired node policy")
	}
	if len(state.Tunnels) > 64 {
		return errors.New("too many managed tunnel contracts")
	}
	seen := make(map[int]struct{}, len(state.Tunnels))
	for _, tunnel := range state.Tunnels {
		if tunnel.ID <= 0 || tunnel.Name == "" || tunnel.Provider == "" || tunnel.TargetPort < 1 || tunnel.TargetPort > 65535 || !validTunnelHostname(tunnel.PublicHostname) {
			return errors.New("invalid managed tunnel contract")
		}
		if _, exists := seen[tunnel.ID]; exists {
			return errors.New("duplicate managed tunnel contract")
		}
		seen[tunnel.ID] = struct{}{}
		if tunnel.Protocol != "http" && tunnel.Protocol != "https" && tunnel.Protocol != "tcp" && tunnel.Protocol != "udp" {
			return errors.New("invalid managed tunnel protocol")
		}
		if !localTarget(tunnel.TargetHost) || tunnel.TargetHost != canonicalTunnelHost(tunnel.TargetHost) {
			return errors.New("managed tunnel target is not local/private")
		}
		if len(tunnel.ProviderConfig) > 16*1024 || (len(tunnel.ProviderConfig) > 0 && !json.Valid(tunnel.ProviderConfig)) {
			return errors.New("invalid managed tunnel provider config")
		}
	}
	return nil
}

func localTarget(host string) bool {
	host = canonicalTunnelHost(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func canonicalTunnelHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func validTunnelHostname(host string) bool {
	if host != canonicalTunnelHost(host) || len(host) == 0 || len(host) > 253 || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r == '-' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}

func randomAgentID() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "pa_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func validAgentID(value string) bool {
	if len(value) < 16 || len(value) > 80 {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if len(message) > 512 {
		return message[:512]
	}
	return message
}
