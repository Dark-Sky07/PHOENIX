package job

import (
	"errors"
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// Uses the isolated CI PostgreSQL database and must not run in parallel with
// tests replacing the process-global connection.
func TestOpenvpnPollReceiptPostgres(t *testing.T) {
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
	row := xray.ClientTraffic{Email: uuid.NewString() + "@openvpn-poll.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	j := newOpenvpnAccounting()
	t.Cleanup(func() {
		if j.captureUnlock != nil {
			j.captureUnlock()
			j.captureUnlock = nil
		}
		if j.pending != nil {
			_ = db.Where("id = ?", j.pending.id).Delete(&model.XrayTrafficInFlight{}).Error
		}
		_ = db.Where("email = ?", row.Email).Delete(&row).Error
	})
	calls := 0
	if _, err := commitOpenvpnPoll(j, func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		calls++
		return nil, []*xray.ClientTraffic{{Email: row.Email, Up: 42, Down: 5}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unexpected poll count %d", calls)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 42 || row.Down != 5 {
		t.Fatalf("wrong usage: %+v %v", row, err)
	}
	partial := errors.New("one daemon failed")
	pollErr, commitErr := commitOpenvpnPoll(j, func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, []*xray.ClientTraffic{{Email: row.Email, Up: 3, Down: 2}}, partial
	})
	if !errors.Is(pollErr, partial) || commitErr != nil {
		t.Fatalf("partial poll not committed separately: poll=%v commit=%v", pollErr, commitErr)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 45 || row.Down != 7 {
		t.Fatalf("wrong partial usage: %+v %v", row, err)
	}
}
