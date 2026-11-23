package job

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestL2TPSampledTrafficRetriesWithJournalAndReceipt(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	client := model.ClientRecord{Email: "l2tp-journal@example.test", Enable: true}
	traffic := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, value := range []any{&client, &traffic} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("CREATE TRIGGER reject_l2tp_traffic BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	collector := NewXrayTrafficJob()
	calls := 0
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		calls++
		return nil, []*xray.ClientTraffic{{Email: client.Email, Up: 123, Down: 456}}, nil
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil {
		t.Fatal("injected commit failure reported success")
	}
	if calls != 1 || collector.pending == nil || collector.captureUnlock == nil {
		t.Fatalf("sample not retained: calls=%d pending=%v", calls, collector.pending != nil)
	}
	batchID := collector.pending.id
	path := filepath.Join(xrayJournalDir(), batchID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	var markers int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("poll marker count %d: %v", markers, err)
	}
	if err := db.Exec("DROP TRIGGER reject_l2tp_traffic").Error; err != nil {
		t.Fatal(err)
	}
	// Simulate a commit with a lost response. The original UUID receipt must
	// suppress the retry, not increment the same L2TP sample a second time.
	if _, _, err := collector.inboundService.AddCheckedXrayTrafficBatchUnderAllocationLock(batchID, collector.pending.traffic, collector.pending.clients, collector.pending.identities); err != nil {
		t.Fatal(err)
	}
	if _, err := persistL2TPPoll(collector, poll); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || collector.pending != nil || collector.captureUnlock != nil {
		t.Fatalf("retry re-polled or failed to clear pending: %d", calls)
	}
	if err := db.First(&traffic, traffic.Id).Error; err != nil || traffic.Up != 123 || traffic.Down != 456 {
		t.Fatalf("sample counted incorrectly: %+v %v", traffic, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("journal not removed: %v", err)
	}
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("poll marker remains: %d %v", markers, err)
	}
	var receipts int64
	if err := db.Model(&model.XrayTrafficReceipt{}).Where("id = ?", batchID).Count(&receipts).Error; err != nil || receipts != 1 {
		t.Fatalf("receipt missing: %d %v", receipts, err)
	}
}

func TestL2TPSampledTrafficJournalReplaysAfterCollectorRestart(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	client := model.ClientRecord{Email: "l2tp-replay@example.test", Enable: true}
	traffic := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, value := range []any{&client, &traffic} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("CREATE TRIGGER reject_l2tp_replay BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	collector := NewXrayTrafficJob()
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, []*xray.ClientTraffic{{Email: client.Email, Up: 88}}, nil
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || collector.pending == nil || collector.captureUnlock == nil {
		t.Fatalf("sample not retained: %v", err)
	}
	id := collector.pending.id
	// Simulate a process dying after the journal has been synced. The lock is
	// released by process exit; the journal and DB marker survive.
	collector.captureUnlock()
	collector.captureUnlock = nil
	collector.pending = nil
	if err := db.Exec("DROP TRIGGER reject_l2tp_replay").Error; err != nil {
		t.Fatal(err)
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&traffic, traffic.Id).Error; err != nil || traffic.Up != 88 {
		t.Fatalf("replay incorrect: %+v %v", traffic, err)
	}
	if _, err := os.Stat(filepath.Join(xrayJournalDir(), id+".json")); !os.IsNotExist(err) {
		t.Fatalf("replayed journal retained: %v", err)
	}
	var count int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker not cleared: %d %v", count, err)
	}
}

func TestL2TPEmptySampleClearsMarkerWithoutReceipt(t *testing.T) {
	setupIntegrationDB(t)
	collector := NewXrayTrafficJob()
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) { return nil, nil, errEmptyL2TPSample }
	if _, err := persistL2TPPoll(collector, poll); err != nil {
		t.Fatal(err)
	}
	if collector.pending != nil || collector.captureUnlock != nil {
		t.Fatal("empty sample retained a batch or lock")
	}
	db := database.GetDB()
	var markers, receipts int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("markers=%d err=%v", markers, err)
	}
	if err := db.Model(&model.XrayTrafficReceipt{}).Count(&receipts).Error; err != nil || receipts != 0 {
		t.Fatalf("receipts=%d err=%v", receipts, err)
	}
}
