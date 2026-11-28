package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestFleetEnrollmentInstallerIsOneTimeAndUsesVerifiedOfficialRelease(t *testing.T) {
	initPhaseBTestDB(t)
	svc := FleetService{}
	created, err := svc.CreateEnrollment(FleetEnrollmentInput{Name: "installer-agent"})
	if err != nil {
		t.Fatal(err)
	}
	installer, err := svc.CreateEnrollmentInstaller(created.Enrollment.ID, FleetInstallerInput{PanelURL: "https://panel.example.com/fleet", CertSHA256: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="})
	if err != nil {
		t.Fatal(err)
	}
	if installer.Enrollment.ID != created.Enrollment.ID || installer.BootstrapExpiresAt <= 0 {
		t.Fatalf("installer metadata = %+v", installer)
	}
	for _, want := range []string{
		"https://api.github.com/repos/$repo/releases/latest",
		"sha256sum --check --status",
		"/usr/local/bin/x-ui agent --once --panel 'https://panel.example.com/fleet'",
		"--bootstrap-token '",
		"ExecStart=/usr/local/bin/x-ui agent --panel https://panel.example.com/fleet",
		"ReadWritePaths=/etc/x-ui /usr/local/bin",
		"Restart=on-failure",
	} {
		if !strings.Contains(installer.Script, want) {
			t.Fatalf("installer is missing %q:\n%s", want, installer.Script)
		}
	}
	if strings.Contains(installer.Script, "PHOENIX_REPO=") || strings.Contains(installer.Script, "--allow-http") {
		t.Fatalf("installer exposed an unsafe source or transport override:\n%s", installer.Script)
	}
	scriptPath := filepath.Join(t.TempDir(), "fleet-installer.sh")
	if err := os.WriteFile(scriptPath, []byte(installer.Script), 0o700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("sh", "-n", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("generated installer has invalid shell syntax: %v\n%s", err, output)
	}
	if _, err := svc.AgentEnroll(context.Background(), AgentEnrollRequest{
		BootstrapToken: created.BootstrapToken, AgentID: "pa_installer_old_token_123", Version: "test", Capabilities: json.RawMessage(`{}`),
	}); err == nil {
		t.Fatal("installer reissue did not invalidate the old bootstrap token")
	}
	newToken := installerBootstrapToken(t, installer.Script)
	if _, err := svc.AgentEnroll(context.Background(), AgentEnrollRequest{
		BootstrapToken: newToken, AgentID: "pa_installer_new_token_123", Version: "test", Capabilities: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("installer bootstrap token was not usable: %v", err)
	}
	if _, err := svc.CreateEnrollmentInstaller(created.Enrollment.ID, FleetInstallerInput{PanelURL: "https://panel.example.com"}); err == nil {
		t.Fatal("installer was reissued after enrollment")
	}
	for _, raw := range []string{"http://panel.example.com", "https://user:pass@panel.example.com", "https://panel.example.com/?x=1", "https://panel.example.com/a%20b"} {
		if _, err := svc.CreateEnrollmentInstaller(999, FleetInstallerInput{PanelURL: raw}); err == nil {
			t.Fatalf("unsafe installer panel URL accepted: %q", raw)
		}
	}
}

func installerBootstrapToken(t *testing.T, script string) string {
	t.Helper()
	const marker = "--bootstrap-token '"
	start := strings.Index(script, marker)
	if start < 0 {
		t.Fatal("installer has no bootstrap token")
	}
	value := script[start+len(marker):]
	end := strings.Index(value, "'")
	if end < 0 {
		t.Fatal("installer bootstrap token is unterminated")
	}
	return value[:end]
}

func TestFleetOfficialReleaseUpdateRolloutIsPinnedAndAcknowledged(t *testing.T) {
	initPhaseBTestDB(t)
	oldResolver := fleetOfficialUpdateResolver
	fleetOfficialUpdateResolver = func(_ context.Context, architecture string) (fleetOfficialUpdate, error) {
		if architecture != "amd64" {
			t.Fatalf("unexpected release architecture: %q", architecture)
		}
		return fleetOfficialUpdate{ReleaseTag: "v5.9.53-phoenix", Architecture: architecture, ArchiveSHA256: strings.Repeat("a", 64)}, nil
	}
	t.Cleanup(func() { fleetOfficialUpdateResolver = oldResolver })

	svc := FleetService{}
	created, err := svc.CreateEnrollment(FleetEnrollmentInput{Name: "update-agent"})
	if err != nil {
		t.Fatal(err)
	}
	agentID := "pa_update_agent_123456789"
	enrolled, err := svc.AgentEnroll(context.Background(), AgentEnrollRequest{
		BootstrapToken: created.BootstrapToken, AgentID: agentID, Version: "phoenix-agent/test",
		Capabilities: json.RawMessage(`{"updates":"official-v1","updateArch":"amd64"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := svc.RequestEnrollmentUpdate(created.Enrollment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != "pending" || queued.DeadlineAt <= queued.RequestedAt || queued.ReleaseTag != "v5.9.53-phoenix" || queued.Architecture != "amd64" || queued.ArchiveSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("pinned update request = %+v", queued)
	}
	checkin, err := svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{
		AgentID: agentID, Version: "phoenix-agent/test", Capabilities: json.RawMessage(`{"updates":"official-v1","updateArch":"amd64"}`),
	})
	if err != nil || checkin.Update == nil || checkin.Update.RequestedAt != queued.RequestedAt || checkin.Update.ReleaseTag != queued.ReleaseTag || checkin.Update.ArchiveSHA256 != queued.ArchiveSHA256 {
		t.Fatalf("update instruction = (%+v, %v)", checkin, err)
	}
	checkin, err = svc.AgentCheckIn(context.Background(), enrolled.Credential, AgentCheckInRequest{
		AgentID: agentID, Version: "phoenix-agent/test", Capabilities: json.RawMessage(`{"updates":"official-v1","updateArch":"amd64"}`),
		Update: &AgentUpdateReport{RequestedAt: queued.RequestedAt, Status: "installed"},
	})
	if err != nil || checkin.Update != nil {
		t.Fatalf("installed update acknowledgement = (%+v, %v)", checkin, err)
	}
	rows, err := svc.ListEnrollments()
	if err != nil || len(rows) != 1 || rows[0].Update == nil || rows[0].Update.Status != "installed" || rows[0].Update.AppliedAt == 0 {
		t.Fatalf("installed update state = (%+v, %v)", rows, err)
	}
	if _, err := svc.RequestEnrollmentUpdate(created.Enrollment.ID); err != nil {
		t.Fatalf("completed update could not be explicitly requeued: %v", err)
	}
	disabled, err := svc.SetEnrollmentEnabled(created.Enrollment.ID, false)
	if err != nil || disabled.Update == nil || disabled.Update.Status != "cancelled" {
		t.Fatalf("disabling enrollment did not cancel pending update: (%+v, %v)", disabled, err)
	}
}

func TestFleetOfficialReleaseUpdateRequiresAgentCapability(t *testing.T) {
	initPhaseBTestDB(t)
	svc := FleetService{}
	created, err := svc.CreateEnrollment(FleetEnrollmentInput{Name: "no-update-agent"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.AgentEnroll(context.Background(), AgentEnrollRequest{BootstrapToken: created.BootstrapToken, AgentID: "pa_no_update_123456789", Version: "test", Capabilities: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RequestEnrollmentUpdate(created.Enrollment.ID); err == nil {
		t.Fatal("agent without official updater capability accepted an update request")
	}
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
