package fleetagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

func TestParsePanelURLRequiresHTTPSByDefault(t *testing.T) {
	for _, raw := range []string{"", "panel.example.com", "http://panel.example.com", "https://user:pass@panel.example.com"} {
		if _, err := parsePanelURL(raw, false); err == nil {
			t.Fatalf("unsafe panel URL accepted: %q", raw)
		}
	}
	if _, err := parsePanelURL("https://panel.example.com/base", false); err != nil {
		t.Fatalf("valid HTTPS panel URL rejected: %v", err)
	}
	if _, err := parsePanelURL("http://127.0.0.1:2053", true); err != nil {
		t.Fatalf("explicit development HTTP URL rejected: %v", err)
	}
}

func TestDesiredStateRejectsPublicTunnelTargets(t *testing.T) {
	state := &service.AgentDesiredState{
		TemplateID: 1,
		Revision:   1,
		Document: service.FleetTemplateDocument{
			SchemaVersion: 1,
			Node:          service.FleetNodePolicy{InboundSyncMode: "all"},
		},
		Tunnels: []service.AgentTunnelDelivery{{
			ID: 1, Name: "public", Provider: "connector", PublicHostname: "app.example.com", TargetHost: "8.8.8.8", TargetPort: 443, Protocol: "tcp",
		}},
	}
	if err := validateDesiredState(state); err == nil {
		t.Fatal("agent accepted public managed tunnel target")
	}
	state.Tunnels[0].TargetHost = "127.0.0.1"
	state.Tunnels[0].ProviderConfig = json.RawMessage(`{"token":"opaque"}`)
	if err := validateDesiredState(state); err != nil {
		t.Fatalf("agent rejected valid local managed tunnel target: %v", err)
	}
	state.Tunnels[0].PublicHostname = "bad host"
	if err := validateDesiredState(state); err == nil {
		t.Fatal("agent accepted unsafe managed tunnel hostname")
	}
}

func TestDesiredStateMatchesIncludesTunnelContract(t *testing.T) {
	first := &service.AgentDesiredState{TemplateID: 1, Revision: 1, Document: service.FleetTemplateDocument{SchemaVersion: 1, Node: service.FleetNodePolicy{InboundSyncMode: "all"}}}
	second := &service.AgentDesiredState{TemplateID: 1, Revision: 1, Document: service.FleetTemplateDocument{SchemaVersion: 1, Node: service.FleetNodePolicy{InboundSyncMode: "all"}}}
	if !desiredStateMatches(first, second) {
		t.Fatal("identical desired states did not match")
	}
	second.Tunnels = []service.AgentTunnelDelivery{{ID: 1, Name: "app", Provider: "connector", PublicHostname: "app.example.com", TargetHost: "127.0.0.1", TargetPort: 3000, Protocol: "http"}}
	if desiredStateMatches(first, second) {
		t.Fatal("changed managed tunnel contract did not trigger staging")
	}
}

func TestAgentOnceStopsAfterEnrollment(t *testing.T) {
	var enrollments int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/panel/agent/v1/enroll" {
			t.Errorf("unexpected request in once mode: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		enrollments++
		_ = json.NewEncoder(w).Encode(service.AgentEnrollResponse{Credential: strings.Repeat("a", 43)})
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "agent.json")
	agent, err := New(Config{PanelURL: server.URL, BootstrapToken: strings.Repeat("b", 43), StatePath: statePath, TunnelDir: t.TempDir(), AllowHTTP: true, PollInterval: 5 * time.Second, Once: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if enrollments != 1 || agent.state.Credential == "" {
		t.Fatalf("once agent did not enroll exactly once: enrollments=%d state=%+v", enrollments, agent.state)
	}
}

func TestOfficialUpdateInstructionIsStrictlyPinned(t *testing.T) {
	valid := &service.AgentUpdateInstruction{
		RequestedAt: 1, Source: "official-release-v1", ReleaseTag: "v5.9.53-phoenix", Architecture: "amd64", ArchiveSHA256: strings.Repeat("a", 64),
	}
	if !validOfficialUpdateInstruction(valid, "amd64") {
		t.Fatal("valid pinned official update instruction was rejected")
	}
	for _, mutate := range []func(*service.AgentUpdateInstruction){
		func(value *service.AgentUpdateInstruction) { value.Source = "https://example.invalid/custom" },
		func(value *service.AgentUpdateInstruction) { value.ReleaseTag = "latest" },
		func(value *service.AgentUpdateInstruction) { value.Architecture = "arm64" },
		func(value *service.AgentUpdateInstruction) { value.ArchiveSHA256 = "not-a-sha256" },
	} {
		copy := *valid
		mutate(&copy)
		if validOfficialUpdateInstruction(&copy, "amd64") {
			t.Fatalf("unsafe update instruction was accepted: %+v", copy)
		}
	}
}

func TestOfficialUpdateConfigHasNoBinaryOverride(t *testing.T) {
	if err := validateOfficialUpdateConfig("/usr/local/bin/x-ui"); err != nil {
		t.Fatalf("official binary path rejected: %v", err)
	}
	for _, binaryPath := range []string{"relative/x-ui", "/tmp/x-ui", "/usr/local/bin/x-ui/../other"} {
		if err := validateOfficialUpdateConfig(binaryPath); err == nil {
			t.Fatalf("unsafe binary path accepted: %q", binaryPath)
		}
	}
}

func TestLocalUpdateArchitectureIsEnumOnly(t *testing.T) {
	config := Config{PanelURL: "http://127.0.0.1:2053", AllowHTTP: true, StatePath: filepath.Join(t.TempDir(), "agent.json"), TunnelDir: t.TempDir(), UpdateBinaryPath: "/usr/local/bin/x-ui", UpdateArchitecture: "armv5"}
	agent, err := New(config)
	if err != nil {
		t.Fatalf("locally pinned official architecture rejected: %v", err)
	}
	defer agent.tunnels.Close()
	if agent.updateArch != "armv5" || !strings.Contains(string(agent.capabilities()), `"updateArch":"armv5"`) {
		t.Fatalf("local update architecture was not reported safely: %q %s", agent.updateArch, agent.capabilities())
	}
	config.UpdateArchitecture = "../../custom"
	if _, err := New(config); err == nil {
		t.Fatal("invalid local update architecture was accepted")
	}
}

func TestAgentRehydratesActiveDesiredStateFromProtectedState(t *testing.T) {
	desired := testDesiredState(3, nil)
	statePath := filepath.Join(t.TempDir(), "agent.json")
	persisted, err := json.Marshal(State{
		AgentID: "pa_rehydrate_123456789", Credential: strings.Repeat("c", 43),
		DesiredState: desired, DesiredActive: true, AppliedRevision: desired.Revision,
		Update: &updateState{RequestedAt: 123, Status: "installed"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, persisted, 0o600); err != nil {
		t.Fatal(err)
	}
	var request service.AgentCheckInRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/panel/agent/v1/checkin" {
			t.Errorf("unexpected agent endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode check-in: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(service.AgentCheckInResponse{DesiredState: desired})
	}))
	defer server.Close()
	agent, err := New(Config{PanelURL: server.URL, StatePath: statePath, TunnelDir: t.TempDir(), AllowHTTP: true, PollInterval: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.tunnels.Close()
	if err := agent.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if request.AppliedRevision != 3 || request.ApplyError != "" || request.Update == nil || request.Update.RequestedAt != 123 || request.Update.Status != "installed" || !agent.state.DesiredActive || agent.state.AppliedRevision != 3 {
		t.Fatalf("rehydrated check-in/state = (%+v, %+v)", request, agent.state)
	}
}

func TestAgentStopsManagedStateWhenPanelDisablesEnrollment(t *testing.T) {
	desired := testDesiredState(2, nil)
	statePath := filepath.Join(t.TempDir(), "agent.json")
	persisted, err := json.Marshal(State{
		AgentID: "pa_disabled_123456789", Credential: strings.Repeat("d", 43),
		DesiredState: desired, DesiredActive: true, AppliedRevision: desired.Revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, persisted, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(service.AgentCheckInResponse{Disabled: true})
	}))
	defer server.Close()
	agent, err := New(Config{PanelURL: server.URL, StatePath: statePath, TunnelDir: t.TempDir(), AllowHTTP: true, PollInterval: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.tunnels.Close()
	if err := agent.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agent.state.DesiredActive || agent.state.ApplyError != "" {
		t.Fatalf("disabled enrollment left managed state active: %+v", agent.state)
	}
}

func TestAgentDoesNotAcknowledgeTunnelRevisionUntilReconcileSucceeds(t *testing.T) {
	desired := testDesiredState(1, []service.AgentTunnelDelivery{{
		ID: 1, Name: "app", Provider: "cloudflared", PublicHostname: "app.example.com",
		TargetHost: "127.0.0.1", TargetPort: 3000, Protocol: "http", ProviderConfig: cloudflaredTestConfig(),
	}})
	var checkin service.AgentCheckInRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/panel/agent/v1/enroll":
			_ = json.NewEncoder(w).Encode(service.AgentEnrollResponse{Credential: strings.Repeat("a", 43), DesiredState: desired})
		case "/panel/agent/v1/checkin":
			if err := json.NewDecoder(r.Body).Decode(&checkin); err != nil {
				t.Errorf("decode check-in: %v", err)
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(service.AgentCheckInResponse{DesiredState: desired})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	agent, err := New(Config{
		PanelURL: server.URL, BootstrapToken: strings.Repeat("b", 43), StatePath: filepath.Join(t.TempDir(), "agent.json"),
		TunnelDir: t.TempDir(), AllowHTTP: true, PollInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.tunnels.Close()
	if err := agent.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agent.state.AppliedRevision != 0 || !agent.state.DesiredActive || agent.state.DesiredState == nil || agent.state.ApplyError == "" {
		t.Fatalf("agent did not retain a retryable unsupported tunnel revision: %+v", agent.state)
	}
	if err := agent.SyncOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if checkin.AppliedRevision != 0 || checkin.ApplyError == "" {
		t.Fatalf("agent falsely acknowledged tunnel revision: %+v", checkin)
	}
}

func testDesiredState(revision int, tunnels []service.AgentTunnelDelivery) *service.AgentDesiredState {
	return &service.AgentDesiredState{
		TemplateID: 1, Revision: revision,
		Document: service.FleetTemplateDocument{SchemaVersion: 1, Node: service.FleetNodePolicy{InboundSyncMode: "all"}},
		Tunnels:  tunnels,
	}
}
