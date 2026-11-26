package service

import (
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// TestOpenVPSessionCheckpointHelper is re-executed by the multi-process test.
// Each helper opens its own PostgreSQL connection, like a separate panel host.
func TestOpenVPSessionCheckpointHelper(t *testing.T) {
	if os.Getenv("PHOENIX_OVPN_SESSION_HELPER") != "1" {
		t.Skip("PostgreSQL OpenVPN session helper")
	}
	rx, err := strconv.ParseInt(os.Getenv("PHOENIX_OVPN_SESSION_RX"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := strconv.ParseInt(os.Getenv("PHOENIX_OVPN_SESSION_TX"), 10, 64)
	if err != nil {
		t.Fatal(err)
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
			Email: os.Getenv("PHOENIX_OVPN_SESSION_EMAIL"), Enable: true,
			OpenVPNSessionID: os.Getenv("PHOENIX_OVPN_SESSION_ID"), OpenVPNInboundID: 125,
			OpenVPNRxBytes: rx, OpenVPNTxBytes: tx,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
}

// Four independent panel processes commit the same final OpenVPN session.
// The PostgreSQL checkpoint, not a process-local mutex, must make that replay
// charge exactly once. This covers cooperating hosts sharing one database; it
// does not authenticate a host or fence a lost advisory-lock session.
func TestOpenVPSessionCheckpointAcrossPostgresProcesses(t *testing.T) {
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

	email := uuid.NewString() + "@openvpn-process.test"
	session := "openvpn:process:" + uuid.NewString()
	row := xray.ClientTraffic{Email: email, Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Where("session_id = ?", session).Delete(&model.OpenVPNSessionCounter{}).Error
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
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(self, "-test.run=^TestOpenVPSessionCheckpointHelper$")
			cmd.Env = append(os.Environ(),
				"PHOENIX_OVPN_SESSION_HELPER=1",
				"PHOENIX_TEST_POSTGRES_DSN="+dsn,
				"PHOENIX_OVPN_SESSION_MARKER="+marker,
				"PHOENIX_OVPN_SESSION_EMAIL="+email,
				"PHOENIX_OVPN_SESSION_ID="+session,
				"PHOENIX_OVPN_SESSION_RX=100",
				"PHOENIX_OVPN_SESSION_TX=200",
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				results <- err
				t.Logf("helper output: %s", out)
				return
			}
			results <- nil
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent process commit: %v", err)
		}
	}

	var current xray.ClientTraffic
	if err := db.Where("email = ?", email).Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.Up != 100 || current.Down != 200 {
		t.Fatalf("four processes charged %d/%d instead of 100/200", current.Up, current.Down)
	}
	var checkpoints []model.OpenVPNSessionCounter
	if err := db.Where("session_id = ?", session).Find(&checkpoints).Error; err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("expected one checkpoint row, got %d", len(checkpoints))
	}
	if _, _, err := (&InboundService{}).AddOpenVPNSessionTrafficUnderAllocationLock(marker,
		[]*xray.ClientTraffic{{
			Email: email, Enable: true,
			OpenVPNSessionID: session, OpenVPNInboundID: 125,
			OpenVPNRxBytes: 90, OpenVPNTxBytes: 200,
		}}); err == nil {
		t.Fatal("decreased replay was accepted across processes")
	}
}
