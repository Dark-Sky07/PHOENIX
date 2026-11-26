package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/openvpn"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// TestOpenVPSessionEvidenceHostHelper is re-executed by the multi-host test.
// Each helper has its own binary folder and therefore its own local evidence
// spool, like a different panel host sharing one PostgreSQL database.
func TestOpenVPSessionEvidenceHostHelper(t *testing.T) {
	if os.Getenv("PHOENIX_OVPN_HOST_HELPER") != "1" {
		t.Skip("PostgreSQL OpenVPN evidence helper")
	}
	t.Setenv("XUI_BIN_FOLDER", os.Getenv("PHOENIX_OVPN_HOST_BIN"))
	samples, err := openvpn.CollectSessionEvidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].InboundID != 126 {
		t.Fatalf("expected one local session sample, got %+v", samples)
	}
	t.Setenv("XUI_DB_TYPE", "postgres")
	t.Setenv("XUI_DB_DSN", os.Getenv("PHOENIX_TEST_POSTGRES_DSN"))
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := database.InitDB(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	_, _, err = (&InboundService{}).AddOpenVPNSessionTrafficUnderAllocationLock(
		os.Getenv("PHOENIX_OVPN_SESSION_MARKER"),
		[]*xray.ClientTraffic{{
			Email: samples[0].Email, Enable: true,
			OpenVPNSessionID: samples[0].Key, OpenVPNInboundID: samples[0].InboundID,
			OpenVPNRxBytes: samples[0].Rx, OpenVPNTxBytes: samples[0].Tx,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
}

// Two hosts with separate local evidence spools collect the same completed
// session and commit it to one database. The durable checkpoint, not a shared
// process mutex or a shared evidence directory, must charge it exactly once.
// This does not authenticate a host or protect a compromised filesystem.
func TestOpenVPSessionEvidenceReplayAcrossPostgresHosts(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	t.Setenv("XUI_DB_TYPE", "postgres")
	t.Setenv("XUI_DB_DSN", dsn)
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := database.InitDB(""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()

	email := uuid.NewString() + "@openvpn-hosts.test"
	session := strings.Repeat("a", 32)
	generation := strings.Repeat("b", 32)
	row := xray.ClientTraffic{Email: email, Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Where("session_id = ?", "openvpn:126:"+generation+":"+session).Delete(&model.OpenVPNSessionCounter{}).Error
		_ = db.Where("email = ?", email).Delete(&xray.ClientTraffic{}).Error
	})
	marker := uuid.NewString()
	if err := BeginXrayPoll(marker); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = FinishXrayPoll(marker) })

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for host := 0; host < 2; host++ {
		bin := t.TempDir()
		records := filepath.Join(bin, "openvpn", "126", "session-evidence", "records")
		if err := os.MkdirAll(records, 0o700); err != nil {
			t.Fatal(err)
		}
		openRecord := fmt.Sprintf("session=%s\ncommon_name=%s\ninbound=126\ngeneration=%s\ntrusted=192.0.2.%d:4000\n",
			session, email, generation, host+10)
		closeRecord := fmt.Sprintf("session=%s\ncommon_name=%s\nrx=100\ntx=200\n", session, email)
		for name, body := range map[string]string{session + ".open": openRecord, session + ".close": closeRecord} {
			if err := os.WriteFile(filepath.Join(records, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(self, "-test.run=^TestOpenVPSessionEvidenceHostHelper$")
		cmd.Env = append(os.Environ(),
			"PHOENIX_OVPN_HOST_HELPER=1",
			"PHOENIX_OVPN_HOST_BIN="+bin,
			"PHOENIX_TEST_POSTGRES_DSN="+dsn,
			"PHOENIX_OVPN_SESSION_MARKER="+marker,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("host %d commit: %v: %s", host, err, out)
		}
	}

	var current xray.ClientTraffic
	if err := db.Where("email = ?", email).Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.Up != 100 || current.Down != 200 {
		t.Fatalf("two hosts charged %d/%d instead of 100/200", current.Up, current.Down)
	}
	var checkpoints []model.OpenVPNSessionCounter
	if err := db.Where("session_id = ?", "openvpn:126:"+generation+":"+session).Find(&checkpoints).Error; err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected one durable checkpoint, got %d", len(checkpoints))
	}
	if _, _, err := (&InboundService{}).AddOpenVPNSessionTrafficUnderAllocationLock(marker,
		[]*xray.ClientTraffic{{
			Email: email, Enable: true,
			OpenVPNSessionID: "openvpn:126:" + generation + ":" + session, OpenVPNInboundID: 126,
			OpenVPNRxBytes: 90, OpenVPNTxBytes: 200,
		}}); err == nil {
		t.Fatal("decreased replay from a second host was accepted")
	}
}
