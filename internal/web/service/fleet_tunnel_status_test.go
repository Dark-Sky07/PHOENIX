package service

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
)

func TestFleetAgentPersistsOnlyAssignedTunnelReports(t *testing.T) {
	initPhaseBTestDB(t)
	svc := FleetService{}
	tunnel, err := svc.CreateTunnel(ManagedTunnelInput{
		Name: "cloudflare-app", Provider: "cloudflared", PublicHostname: "app.example.com",
		TargetHost: "127.0.0.1", TargetPort: 3000, Protocol: "http",
		ProviderConfig: json.RawMessage(`{"tunnelId":"11111111-1111-4111-8111-111111111111","credentials":{"AccountTag":"account","TunnelSecret":"secret","TunnelID":"11111111-1111-4111-8111-111111111111"}}`),
		Enabled:        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	document := json.RawMessage(`{"schemaVersion":1,"node":{"inboundSyncMode":"all"},"managedTunnelIds":[` + strconv.Itoa(tunnel.ID) + `]}`)
	template, err := svc.CreateTemplate(FleetTemplateInput{Name: "edge", Document: document})
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := svc.CreateEnrollment(FleetEnrollmentInput{Name: "edge-agent"})
	if err != nil {
		t.Fatal(err)
	}
	agentID := "pa_tunnel_status_123456"
	enrolled, err := svc.AgentEnroll(context.Background(), AgentEnrollRequest{
		BootstrapToken: enrollment.BootstrapToken, AgentID: agentID, Version: "test", Capabilities: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Deploy(template.ID, FleetDeploymentInput{Revision: 1, EnrollmentIDs: []int{enrollment.Enrollment.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{
		AgentID: agentID, Version: "test", Capabilities: json.RawMessage(`{}`), ApplyError: "cloudflared unavailable",
		Tunnels: []AgentTunnelReport{{TunnelID: tunnel.ID, Provider: "cloudflared", Status: "unsupported"}},
	}); err != nil {
		t.Fatalf("agent could not report partial desired state before acknowledgement: %v", err)
	}
	if _, err := svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{
		AgentID: agentID, Version: "test", Capabilities: json.RawMessage(`{}`), AppliedRevision: 1,
		Tunnels: []AgentTunnelReport{{TunnelID: tunnel.ID, Provider: "cloudflared", Status: "running", PID: 4242}},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := svc.ListEnrollments()
	if err != nil || len(rows) != 1 || len(rows[0].TunnelStatuses) != 1 {
		t.Fatalf("fleet tunnel status = (%+v, %v)", rows, err)
	}
	status := rows[0].TunnelStatuses[0]
	if status.TunnelID != tunnel.ID || status.Provider != "cloudflared" || status.Status != "running" || status.PID != 4242 {
		t.Fatalf("stored tunnel status = %+v", status)
	}
	if _, err := svc.CreateRevision(template.ID, FleetTemplateRevisionInput{Document: json.RawMessage(`{"schemaVersion":1,"node":{"inboundSyncMode":"all"}}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Deploy(template.ID, FleetDeploymentInput{Revision: 2, EnrollmentIDs: []int{enrollment.Enrollment.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{
		AgentID: agentID, Version: "test", Capabilities: json.RawMessage(`{}`), AppliedRevision: 1,
		Tunnels: []AgentTunnelReport{{TunnelID: tunnel.ID, Provider: "cloudflared", Status: "running", PID: 4242}},
	}); err != nil {
		t.Fatalf("agent could not report its still-applied tunnel while receiving a rollout: %v", err)
	}
	rows, err = svc.ListEnrollments()
	if err != nil || len(rows) != 1 || len(rows[0].TunnelStatuses) != 0 {
		t.Fatalf("stale tunnel status remained visible after a new desired revision: (%+v, %v)", rows, err)
	}
	if _, err := svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{
		AgentID: agentID, Version: "test", Capabilities: json.RawMessage(`{}`), AppliedRevision: 1,
		Tunnels: []AgentTunnelReport{{TunnelID: tunnel.ID + 10, Provider: "cloudflared", Status: "running"}},
	}); err == nil {
		t.Fatal("agent was allowed to report an unassigned tunnel")
	}
	if _, err := svc.SetEnrollmentEnabled(enrollment.Enrollment.ID, false); err != nil {
		t.Fatal(err)
	}
	disabled, err := svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{AgentID: agentID})
	if err != nil || !disabled.Disabled || disabled.DesiredState != nil {
		t.Fatalf("disabled agent terminal response = (%+v, %v)", disabled, err)
	}
}
