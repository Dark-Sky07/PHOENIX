// Package fleetagent implements PHOENIX's outbound-only fleet agent. It has no
// listener and accepts no commands: it polls the control plane, validates the
// narrow desired-state document, and persists it with owner-only permissions.
package fleetagent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

const (
	defaultPollInterval = 15 * time.Second
	maxAgentResponse    = 128 * 1024
)

type Config struct {
	PanelURL        string
	BootstrapToken  string
	StatePath       string
	TunnelDir       string
	CloudflaredPath string
	PollInterval    time.Duration
	CertSHA256      string
	AllowHTTP       bool
}

type State struct {
	AgentID         string                     `json:"agentId"`
	Credential      string                     `json:"credential"`
	DesiredState    *service.AgentDesiredState `json:"desiredState,omitempty"`
	DesiredActive   bool                       `json:"desiredActive"`
	AppliedRevision int                        `json:"appliedRevision"`
	ApplyError      string                     `json:"applyError,omitempty"`
	UpdatedAt       int64                      `json:"updatedAt"`
}

type Agent struct {
	config  Config
	client  *http.Client
	state   State
	tunnels *tunnelManager
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
	a := &Agent{config: config, client: client, tunnels: tunnels}
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
	ticker := time.NewTicker(a.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := a.SyncOnce(ctx); err != nil {
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
		Version:         "phoenix-agent/1",
		Capabilities:    a.capabilities(),
		AppliedRevision: a.state.AppliedRevision,
		ApplyError:      a.state.ApplyError,
		Tunnels:         a.tunnels.Reports(),
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
	return a.saveState()
}

func (a *Agent) enroll(ctx context.Context) error {
	request := service.AgentEnrollRequest{
		BootstrapToken: a.config.BootstrapToken,
		AgentID:        a.state.AgentID,
		Version:        "phoenix-agent/1",
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

func (a *Agent) capabilities() json.RawMessage {
	managed := "contract-v1"
	if a.tunnels.SupportsCloudflared() {
		managed = "cloudflared-v1"
	}
	return json.RawMessage(fmt.Sprintf(`{"desiredState":"apply-v1","managedTunnels":%q}`, managed))
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
