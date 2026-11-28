package fleetagent

import (
	"encoding/json"
	"testing"

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
