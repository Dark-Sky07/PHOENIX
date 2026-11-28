package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func fleetTestDocument() json.RawMessage {
	return json.RawMessage(`{
		"schemaVersion": 1,
		"labels": {"region": "de-fra"},
		"node": {"inboundSyncMode": "all"}
	}`)
}

func TestFleetTemplateDocumentIsStrictAndCanonical(t *testing.T) {
	valid, canonical, digest, err := normalizeFleetDocument(fleetTestDocument())
	if err != nil {
		t.Fatal(err)
	}
	if valid.SchemaVersion != 1 || canonical == "" || len(digest) != 64 {
		t.Fatalf("canonical document = (%+v, %q, %q)", valid, canonical, digest)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"schemaVersion":1,"node":{"inboundSyncMode":"all"},"command":"curl evil | sh"}`),
		json.RawMessage(`{"schemaVersion":1,"node":{"inboundSyncMode":"selected"}}`),
		json.RawMessage(`{"schemaVersion":2,"node":{"inboundSyncMode":"all"}}`),
		json.RawMessage(`{"schemaVersion":1,"node":{"inboundSyncMode":"all"}} {}`),
	} {
		if _, _, _, err := normalizeFleetDocument(raw); err == nil {
			t.Fatalf("unsafe or invalid fleet document accepted: %s", raw)
		}
	}
}

func TestFleetAgentEnrollmentDeploymentAndAcknowledgement(t *testing.T) {
	initPhaseBTestDB(t)
	svc := FleetService{}
	template, err := svc.CreateTemplate(FleetTemplateInput{Name: "fra-edge", Document: fleetTestDocument()})
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := svc.CreateEnrollment(FleetEnrollmentInput{Name: "fra-agent-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(enrollment.BootstrapToken) < 32 || enrollment.Enrollment.HasCredential {
		t.Fatalf("unexpected enrollment response: %+v", enrollment)
	}
	agentID := "pa_test_agent_123456789"
	enrolled, err := svc.AgentEnroll(context.Background(), AgentEnrollRequest{
		BootstrapToken: enrollment.BootstrapToken,
		AgentID:        agentID,
		Version:        "phoenix-agent/test",
		Capabilities:   json.RawMessage(`{"desiredState":"stage-only"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(enrolled.Credential) < 32 || enrolled.DesiredState != nil {
		t.Fatalf("unexpected agent enrollment response: %+v", enrolled)
	}
	if _, err := svc.AgentEnroll(context.Background(), AgentEnrollRequest{
		BootstrapToken: enrollment.BootstrapToken,
		AgentID:        agentID,
		Version:        "phoenix-agent/test",
		Capabilities:   json.RawMessage(`{}`),
	}); !errors.Is(err, ErrFleetAgentUnauthorized()) {
		t.Fatalf("bootstrap token replay error = %v", err)
	}

	if _, err := svc.Deploy(template.ID, FleetDeploymentInput{Revision: 1, EnrollmentIDs: []int{enrollment.Enrollment.ID}}); err != nil {
		t.Fatal(err)
	}
	checkin, err := svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{
		AgentID: agentID, Version: "phoenix-agent/test", Capabilities: json.RawMessage(`{}`),
	})
	if err != nil || checkin.DesiredState == nil || checkin.DesiredState.Revision != 1 {
		t.Fatalf("first check-in = (%+v, %v)", checkin, err)
	}
	if _, err := svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{
		AgentID: agentID, Version: "phoenix-agent/test", Capabilities: json.RawMessage(`{}`), AppliedRevision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var stored model.FleetAgentEnrollment
	if err := database.GetDB().First(&stored, enrollment.Enrollment.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.CredentialHash == "" || stored.CredentialHash == enrolled.Credential || stored.BootstrapTokenHash == enrollment.BootstrapToken {
		t.Fatal("fleet credentials were stored in plaintext")
	}
	rows, err := svc.ListEnrollments()
	if err != nil || len(rows) != 1 || rows[0].Assignment == nil || rows[0].Assignment.Status != "applied" || rows[0].Assignment.AppliedRevision != 1 {
		t.Fatalf("fleet acknowledgement not persisted: (%+v, %v)", rows, err)
	}
}

func TestFleetPoolBindsTemplateAndSelectsCohort(t *testing.T) {
	initPhaseBTestDB(t)
	svc := FleetService{}
	template, err := svc.CreateTemplate(FleetTemplateInput{Name: "fra-edge", Document: fleetTestDocument()})
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.CreateEnrollment(FleetEnrollmentInput{Name: "fra-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.CreateEnrollment(FleetEnrollmentInput{Name: "fra-b"})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := svc.CreatePool(FleetPoolInput{Name: "fra-canary", TemplateID: &template.ID, EnrollmentIDs: []int{first.Enrollment.ID, second.Enrollment.ID}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Deploy(template.ID, FleetDeploymentInput{Revision: 1, PoolID: pool.ID}); err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateTemplate(FleetTemplateInput{Name: "other", Document: fleetTestDocument()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Deploy(other.ID, FleetDeploymentInput{Revision: 1, PoolID: pool.ID}); err == nil {
		t.Fatal("pool template binding did not block a different template")
	}
}

func TestManagedTunnelRejectsPublicTargetAndRedactsProviderConfig(t *testing.T) {
	initPhaseBTestDB(t)
	if _, err := normalizeManagedTunnel(ManagedTunnelInput{
		Name: "bad", Provider: "connector", PublicHostname: "app.example.com", TargetHost: "8.8.8.8", TargetPort: 443, Protocol: "tcp", Enabled: true,
	}, nil); err == nil {
		t.Fatal("public managed tunnel target was accepted")
	}
	svc := FleetService{}
	created, err := svc.CreateTunnel(ManagedTunnelInput{
		Name: "app", Provider: "connector", PublicHostname: "app.example.com", TargetHost: "127.0.0.1", TargetPort: 3000, Protocol: "http", ProviderConfig: json.RawMessage(`{"token":"not-in-a-view"}`), Enabled: true,
	})
	if err != nil || !created.HasProviderConfig {
		t.Fatalf("create managed tunnel = (%+v, %v)", created, err)
	}
	rows, err := svc.ListTunnels()
	if err != nil || len(rows) != 1 || !rows[0].HasProviderConfig {
		t.Fatalf("managed tunnel view = (%+v, %v)", rows, err)
	}
	body, _ := json.Marshal(rows[0])
	if strings.Contains(string(body), "not-in-a-view") || strings.Contains(string(body), "providerConfig") {
		t.Fatalf("managed tunnel API view leaked provider config: %s", body)
	}
}
