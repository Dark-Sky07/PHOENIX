package service

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func writeOpenVPNEvidenceFixture(t *testing.T, inbound int, fields string, name string) string {
	t.Helper()
	path := filepath.Join(os.Getenv("XUI_BIN_FOLDER"), "openvpn", strconv.Itoa(inbound), "session-evidence", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fields), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenVPSessionEvidenceFencesQuotaAndRemoval(t *testing.T) {
	setupBulkDB(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	email := "guarded@example.test"
	openPath := writeOpenVPNEvidenceFixture(t, 751,
		"session=aaaaaaaaaaaaaaaa\ncommon_name="+email+"\ninbound=751\ngeneration="+strings.Repeat("a", 32)+"\n",
		"records/aaaaaaaaaaaaaaaa.open")

	if err := rejectUnverifiedOpenVPNReset([]string{email}); err == nil {
		t.Fatal("open session did not fence reset")
	}
	if err := rejectUnverifiedOpenVPNReset([]string{"other@example.test"}); err != nil {
		t.Fatalf("unrelated identity blocked by open session: %v", err)
	}
	if err := rejectUnprovenDaemonRemoval(database.GetDB(), []string{email}, nil); err == nil {
		t.Fatal("open session did not fence removal")
	}
	if err := (&InboundService{}).ResetClientTrafficByEmail(email); err == nil {
		t.Fatal("reset succeeded with unverified session")
	}

	incidentPath := writeOpenVPNEvidenceFixture(t, 751, "hook failure\n", "incidents/failure.1")
	if err := rejectUnverifiedOpenVPNReset(nil); err == nil {
		t.Fatal("incident did not fence global reset")
	}
	if err := rejectUnprovenDaemonRemoval(database.GetDB(), nil, nil); err == nil {
		t.Fatal("incident did not fence removal")
	}
	if err := os.Remove(openPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(incidentPath); err != nil {
		t.Fatal(err)
	}
	if err := rejectUnverifiedOpenVPNReset([]string{email}); err != nil {
		t.Fatalf("resolved evidence still fences reset: %v", err)
	}
	if err := rejectUnprovenDaemonRemoval(database.GetDB(), []string{email}, nil); err != nil {
		t.Fatalf("resolved evidence still fences removal: %v", err)
	}
}

func TestQuotaReductionFencedByOpenVPSessionEvidence(t *testing.T) {
	setupBulkDB(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	email := "quota-guard@example.test"
	inbound := mkInbound(t, 752, model.OpenVPN, `{"clients":[]}`)
	client := model.Client{
		Email:   email,
		ID:      "1b2c3d4e-5f60-4718-8293-a4b5c6d7e8f9",
		SubID:   "quota-guard-sub",
		TotalGB: 300,
		Enable:  true,
	}
	inbound.Settings = clientsSettings(t, []model.Client{client})
	if err := database.GetDB().Save(inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := (&ClientService{}).SyncInbound(nil, inbound.Id, []model.Client{client}); err != nil {
		t.Fatal(err)
	}
	writeOpenVPNEvidenceFixture(t, 752,
		"session=bbbbbbbbbbbbbbbb\ncommon_name="+email+"\ninbound=752\ngeneration="+strings.Repeat("b", 32)+"\n",
		"records/bbbbbbbbbbbbbbbb.open")
	rec, err := (&ClientService{}).GetRecordByEmail(nil, email)
	if err != nil || rec == nil {
		t.Fatalf("find client record: %v", err)
	}
	updated := client
	updated.TotalGB = client.TotalGB - 1
	if _, err := (&ClientService{}).Update(nil, rec.Id, updated); err == nil {
		t.Fatal("quota reduction succeeded with unverified session")
	}
}
