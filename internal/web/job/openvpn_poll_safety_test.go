package job

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestOpenvpnPollRetainsExactBatchAndMarkerOnFailure(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "openvpn-poll@example.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_openvpn BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected'); END").Error; err != nil {
		t.Fatal(err)
	}
	j := newOpenvpnAccounting()
	t.Cleanup(func() {
		if j.captureUnlock != nil {
			j.captureUnlock()
			j.captureUnlock = nil
		}
	})
	calls := 0
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		calls++
		var count int64
		if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("poll lacked marker: %d %v", count, err)
		}
		return nil, []*xray.ClientTraffic{{Email: row.Email, InboundId: 4, Up: 123, Down: 56}}, nil
	}
	if err := commitOpenvpnPoll(j, poll); err == nil || j.pending == nil || j.captureUnlock == nil {
		t.Fatalf("failed batch not retained: %v", err)
	}
	id := j.pending.id
	path := filepath.Join(xrayJournalDir(), id+".manual")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	if err := service.VerifyXrayPollMarker(id); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 0 {
		t.Fatalf("failed batch wrote traffic: %+v %v", row, err)
	}
	if err := db.Exec("DROP TRIGGER reject_openvpn").Error; err != nil {
		t.Fatal(err)
	}
	if err := commitOpenvpnPoll(j, poll); err != nil || calls != 1 || j.pending != nil || j.captureUnlock != nil {
		t.Fatalf("retry failed or repolled: calls=%d err=%v", calls, err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 123 || row.Down != 56 {
		t.Fatalf("wrong committed traffic: %+v %v", row, err)
	}
	if err := service.VerifyXrayPollMarker(id); err == nil {
		t.Fatal("marker not cleared")
	}
	var count int64
	if err := db.Model(&model.XrayTrafficReceipt{}).Where("id = ?", id).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("receipt missing: %d %v", count, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("journal retained after commit: %v", err)
	}
}

func TestOpenvpnPollRefusesForeignMarker(t *testing.T) {
	setupIntegrationDB(t)
	if err := service.BeginXrayPoll("foreign-test-marker"); err != nil {
		t.Fatal(err)
	}
	j := newOpenvpnAccounting()
	calls := 0
	err := commitOpenvpnPoll(j, func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		calls++
		return nil, nil, errors.New("unexpected poll")
	})
	if err == nil || calls != 0 {
		t.Fatalf("foreign marker allowed poll: calls=%d err=%v", calls, err)
	}
	if err := service.VerifyXrayPollMarker("foreign-test-marker"); err != nil {
		t.Fatalf("foreign evidence removed: %v", err)
	}
}

func TestOpenvpnUncommittedJournalCannotReplayAfterRestart(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "openvpn-restart@example.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_restart BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected'); END").Error; err != nil {
		t.Fatal(err)
	}
	j := newOpenvpnAccounting()
	if err := commitOpenvpnPoll(j, func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, []*xray.ClientTraffic{{Email: row.Email, Up: 91}}, nil
	}); err == nil {
		t.Fatal("expected failure")
	}
	id := j.pending.id
	if j.captureUnlock == nil {
		t.Fatal("allocation lock not held")
	}
	j.captureUnlock()
	j.captureUnlock = nil
	if err := db.Exec("DROP TRIGGER reject_restart").Error; err != nil {
		t.Fatal(err)
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err == nil {
		t.Fatal("manual OpenVPN evidence was replayed")
	}
	if err := service.VerifyXrayPollMarker(id); err != nil {
		t.Fatalf("marker lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(xrayJournalDir(), id+".manual")); err != nil {
		t.Fatalf("manual evidence lost: %v", err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 0 {
		t.Fatalf("usage applied despite missing generation proof: %+v %v", row, err)
	}
}
