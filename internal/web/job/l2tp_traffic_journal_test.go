package job

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/l2tp"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
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

func TestL2TPCounterDecreaseStoresIncidentBeforeClearingPollMarker(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	collector := NewXrayTrafficJob()
	// A previous warning must not swallow the separate counter evidence.
	incidentPath := filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-incident")
	if err := os.WriteFile(incidentPath, []byte("prior orphan\n"), 0600); err != nil {
		t.Fatal(err)
	}
	decrease := &l2tp.CounterDecrease{InboundID: 4, Interface: "ppp0", Email: "alice@example.test", PreviousRx: 100, PreviousTx: 200, ObservedRx: 3, ObservedTx: 210}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) { return nil, nil, decrease }
	if _, err := persistL2TPPoll(collector, poll); err == nil {
		t.Fatal("decrease reported success")
	}
	var markers, incidents int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("marker not cleared after durable incident: %d %v", markers, err)
	}
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&incidents).Error; err != nil || incidents != 1 {
		t.Fatalf("shared incident missing: %d %v", incidents, err)
	}
	files, err := os.ReadDir(filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-evidence"))
	if err != nil || len(files) != 1 {
		t.Fatalf("local evidence missing: %v %v", files, err)
	}
	evidence, err := os.ReadFile(filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-evidence", files[0].Name()))
	if err != nil || !strings.Contains(string(evidence), decrease.Error()) {
		t.Fatalf("counter observation not retained: %q %v", evidence, err)
	}
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatalf("unrelated poll lock blocked: %v", err)
	}
	unlock()
}

func TestL2TPCounterDecreaseRetainsMarkerOnIncidentWriteFailure(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_l2tp_counter BEFORE INSERT ON reseller_node_counter_incidents BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	collector := NewXrayTrafficJob()
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, nil, &l2tp.CounterDecrease{InboundID: 4, Interface: "ppp0", Email: "alice", PreviousRx: 10, ObservedRx: 1}
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || !strings.Contains(err.Error(), "poll marker") {
		t.Fatalf("write failure did not retain marker: %v", err)
	}
	var count int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("missing pending poll marker: %d %v", count, err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("marker did not block subsequent allocation")
	}
	// Retry the independent incident write under the recovery lock; the poll
	// marker remains for manual reconciliation and no sample is fabricated.
	if err := db.Exec("DROP TRIGGER reject_l2tp_counter").Error; err != nil {
		t.Fatal(err)
	}
	unlock, err := service.LockResellerAllocationForXrayRecovery()
	if err != nil {
		t.Fatal(err)
	}
	err = service.RecordL2TPCounterDecreaseUnderAllocationLock(&l2tp.CounterDecrease{InboundID: 4, Interface: "ppp0", Email: "alice", PreviousRx: 10, ObservedRx: 1})
	unlock()
	if err != nil {
		t.Fatalf("incident retry failed: %v", err)
	}
}

func TestL2TPCounterDecreaseRetainsMarkerOnEvidenceWriteFailure(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	path := filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-evidence")
	if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	collector := NewXrayTrafficJob()
	decrease := &l2tp.CounterDecrease{InboundID: 5, Interface: "ppp2", Email: "bob", PreviousTx: 20, ObservedTx: 5}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) { return nil, nil, decrease }
	if _, err := persistL2TPPoll(collector, poll); err == nil || !strings.Contains(err.Error(), "poll marker") {
		t.Fatalf("evidence failure did not retain marker: %v", err)
	}
	var count int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("poll marker missing: %d %v", count, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	unlock, err := service.LockResellerAllocationForXrayRecovery()
	if err != nil {
		t.Fatal(err)
	}
	err = service.RecordL2TPCounterDecreaseUnderAllocationLock(decrease)
	unlock()
	if err != nil {
		t.Fatalf("failed evidence retry: %v", err)
	}
}
