package service

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// Replays of a committed OpenVPN final counter must stay idempotent when the
// checkpoint lives in PostgreSQL. This uses the isolated CI database and does
// not touch SQLite or process-local fallback files.
func TestOpenVPSessionCountersIdempotentPostgres(t *testing.T) {
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

	email := uuid.NewString() + "@openvpn-session-pg.test"
	row := xray.ClientTraffic{Email: email, Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	session := "openvpn:pg:" + uuid.NewString()
	t.Cleanup(func() {
		_ = db.Where("session_id = ?", session).Delete(&model.OpenVPNSessionCounter{}).Error
		_ = db.Where("email = ?", email).Delete(&xray.ClientTraffic{}).Error
	})
	marker := uuid.NewString()
	if err := BeginXrayPoll(marker); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := FinishXrayPoll(marker); err != nil {
			t.Logf("finish poll marker: %v", err)
		}
	})

	sample := func(rx, tx int64) []*xray.ClientTraffic {
		return []*xray.ClientTraffic{{
			Email: email, Enable: true,
			OpenVPNSessionID: session, OpenVPNInboundID: 124,
			OpenVPNRxBytes: rx, OpenVPNTxBytes: tx,
		}}
	}
	service := &InboundService{}
	if _, _, err := service.AddOpenVPNSessionTrafficUnderAllocationLock(marker, sample(100, 200)); err != nil {
		t.Fatalf("commit final session: %v", err)
	}
	if _, _, err := service.AddOpenVPNSessionTrafficUnderAllocationLock(marker, sample(100, 200)); err != nil {
		t.Fatalf("replay final session: %v", err)
	}
	var current xray.ClientTraffic
	if err := db.Where("email = ?", email).Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.Up != 100 || current.Down != 200 {
		t.Fatalf("replay changed PostgreSQL usage: %d/%d", current.Up, current.Down)
	}
	if _, _, err := service.AddOpenVPNSessionTrafficUnderAllocationLock(marker, sample(90, 200)); err == nil {
		t.Fatal("decreased PostgreSQL session counter accepted")
	}
	if err := db.Where("email = ?", email).Take(&current).Error; err != nil {
		t.Fatal(err)
	}
	if current.Up != 100 || current.Down != 200 {
		t.Fatalf("rejected replay changed usage: %d/%d", current.Up, current.Down)
	}
}
