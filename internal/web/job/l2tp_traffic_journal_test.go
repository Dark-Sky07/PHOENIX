package job

import (
	"errors"
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

func TestL2TPIncidentPublicationFailureDefersReconciliationAfterPoll(t *testing.T) {
	setupIntegrationDB(t)
	collector := NewXrayTrafficJob()
	incident := errors.New("shared incident unavailable")
	polls := 0
	publish := func() error { return incident }
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		polls++
		return nil, nil, errEmptyL2TPSample
	}
	if _, err := pollL2TPWithIncidentBarrier(collector, publish, poll); !errors.Is(err, incident) || polls != 1 {
		t.Fatalf("publication error ignored or sample skipped: polls=%d err=%v", polls, err)
	}
	if _, err := pollL2TPWithIncidentBarrier(collector, func() error { return nil }, poll); err != nil || polls != 2 {
		t.Fatalf("reconciliation did not resume after successful publication: polls=%d err=%v", polls, err)
	}
}

func TestL2TPIncidentDuringPollDefersReconciliation(t *testing.T) {
	setupIntegrationDB(t)
	collector := NewXrayTrafficJob()
	incident := errors.New("incident appeared during poll")
	pollComplete := false
	calls := 0
	publish := func() error {
		calls++
		if pollComplete {
			return incident
		}
		return nil
	}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		pollComplete = true
		return nil, nil, errEmptyL2TPSample
	}
	if _, err := pollL2TPWithIncidentBarrier(collector, publish, poll); !errors.Is(err, incident) || calls != 2 {
		t.Fatalf("new incident did not defer reconciliation: calls=%d err=%v", calls, err)
	}
}

func TestL2TPLocalIncidentDefersReconciliationAfterEmptyPoll(t *testing.T) {
	setupIntegrationDB(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	path := filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-incident")
	if err := os.WriteFile(path, []byte("unreconciled\n"), 0600); err != nil {
		t.Fatal(err)
	}
	collector := NewXrayTrafficJob()
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, nil, errEmptyL2TPSample
	}
	if _, err := pollL2TPWithIncidentBarrier(collector, service.PublishL2TPAccountingIncident, poll); err == nil {
		t.Fatal("local incident allowed daemon reconciliation")
	}
	var incidents int64
	if err := database.GetDB().Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&incidents).Error; err != nil || incidents != 1 {
		t.Fatalf("shared incident not published: %d %v", incidents, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("local incident removed: %v", err)
	}
}

func TestL2TPSharedIncidentDefersReconciliationWithoutLocalEvidence(t *testing.T) {
	setupIntegrationDB(t)
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	incident := model.ResellerNodeCounterIncident{NodeId: -2, Email: "", Direction: "l2tp-session-disappeared"}
	if err := database.GetDB().Create(&incident).Error; err != nil {
		t.Fatal(err)
	}
	polls := 0
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		polls++
		return nil, nil, errEmptyL2TPSample
	}
	collector := NewXrayTrafficJob()
	if _, err := pollL2TPWithIncidentBarrier(collector, service.PublishL2TPAccountingIncident, poll); err == nil || !strings.Contains(err.Error(), "shared L2TP") || polls != 1 {
		t.Fatalf("shared incident failed to defer reconciliation: polls=%d err=%v", polls, err)
	}
	// This removal represents simulated manual reconciliation, not automatic recovery.
	if err := database.GetDB().Delete(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := pollL2TPWithIncidentBarrier(collector, service.PublishL2TPAccountingIncident, poll); err != nil {
		t.Fatalf("reconciliation did not resume after reviewed shared incident: %v", err)
	}
}

func TestL2TPIncidentPublicationWaitsForMarkerRestoration(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	id := "restore-before-publish"
	if err := db.Create(&model.XrayTrafficInFlight{ID: id}).Error; err != nil {
		t.Fatal(err)
	}
	collector := NewXrayTrafficJob()
	collector.l2tpHookPollID = id
	collector.l2tpRestorePoll = true
	unlocked := false
	collector.captureUnlock = func() { unlocked = true }
	incident := errors.New("publication still blocked")
	publish := func() error {
		if !unlocked || collector.l2tpRestorePoll {
			t.Fatal("publication attempted while restoration still holds the lock")
		}
		return incident
	}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, nil, errEmptyL2TPSample
	}
	if _, err := pollL2TPWithIncidentBarrier(collector, publish, poll); !errors.Is(err, incident) || !unlocked {
		t.Fatalf("restoration skipped publication barrier: unlocked=%v err=%v", unlocked, err)
	}
}

func TestL2TPIncidentPublicationWaitsForPendingBatchLock(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	client := model.ClientRecord{Email: "l2tp-barrier@example.test", Enable: true}
	traffic := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, value := range []any{&client, &traffic} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("CREATE TRIGGER reject_barrier_batch BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	collector := NewXrayTrafficJob()
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, []*xray.ClientTraffic{{Email: client.Email, Up: 42}}, nil
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || collector.pending == nil || collector.captureUnlock == nil {
		t.Fatalf("batch failure did not retain allocation lock: %v", err)
	}
	if err := db.Exec("DROP TRIGGER reject_barrier_batch").Error; err != nil {
		t.Fatal(err)
	}
	incident := errors.New("incident after batch capture")
	publish := func() error {
		if collector.captureUnlock != nil || collector.pending != nil {
			t.Fatal("publication attempted before batch released its lock")
		}
		// This would deadlock if the collector still owned its allocation lock.
		unlock, err := service.LockResellerAllocationForXrayRecovery()
		if err != nil {
			return err
		}
		unlock()
		return incident
	}
	if _, err := pollL2TPWithIncidentBarrier(collector, publish, func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		t.Fatal("pending batch was repolled")
		return nil, nil, nil
	}); !errors.Is(err, incident) {
		t.Fatalf("publication barrier not enforced after pending batch: %v", err)
	}
	if err := db.First(&traffic, traffic.Id).Error; err != nil || traffic.Up != 42 {
		t.Fatalf("pending batch not committed: %+v %v", traffic, err)
	}
}

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

func TestL2TPMissingTrafficDestinationKeepsJournalAndMarker(t *testing.T) {
	setupIntegrationDB(t)
	collector := NewXrayTrafficJob()
	polls := 0
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		polls++
		return nil, []*xray.ClientTraffic{{Email: "missing-l2tp@example.test", Up: 88}}, nil
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || collector.pending == nil || collector.captureUnlock == nil {
		t.Fatalf("missing destination did not retain batch and lock: %v", err)
	}
	id := collector.pending.id
	if err := service.VerifyXrayPollMarker(id); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	path := filepath.Join(xrayJournalDir(), id+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || polls != 1 {
		t.Fatalf("retry lost sample or polled again: polls=%d err=%v", polls, err)
	}
	// Simulate process death: the journal and marker must survive lock release.
	collector.captureUnlock()
	collector.captureUnlock = nil
	if err := NewXrayTrafficJob().replayXrayJournal(); err == nil {
		t.Fatal("restart discarded missing-destination sample")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("replay removed journal: %v", err)
	}
	if err := service.VerifyXrayPollMarker(id); err != nil {
		t.Fatalf("replay removed marker: %v", err)
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

func TestL2TPUntrackedRuntimeCollectorFencesSharedAllocation(t *testing.T) {
	for _, rejectIncident := range []bool{false, true} {
		t.Run(map[bool]string{false: "incident-written", true: "incident-write-fails"}[rejectIncident], func(t *testing.T) {
			setupIntegrationDB(t)
			bin := t.TempDir()
			t.Setenv("XUI_BIN_FOLDER", bin)
			orphan := filepath.Join(bin, "l2tp", "8", "sessions")
			if err := os.MkdirAll(orphan, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(orphan, "ppp8")
			if err := os.WriteFile(marker, []byte("alice@example.test\n"), 0600); err != nil {
				t.Fatal(err)
			}
			db := database.GetDB()
			if rejectIncident {
				if err := db.Exec("CREATE TRIGGER reject_orphan_incident BEFORE INSERT ON reseller_node_counter_incidents BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
					t.Fatal(err)
				}
				// Reset the process-level failure latch even if an assertion fails.
				t.Cleanup(func() {
					_ = db.Exec("DROP TRIGGER IF EXISTS reject_orphan_incident").Error
					if unlock, err := service.LockResellerAllocationForXrayRecovery(); err == nil {
						_ = service.RecordL2TPSessionReadFailureUnderAllocationLock(&l2tp.SessionReadFailure{Cause: "test cleanup"})
						unlock()
					}
				})
			}
			mgr := &l2tp.Manager{}
			poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
				_, _, err := mgr.CollectTrafficChecked()
				return nil, nil, err
			}
			if _, err := persistL2TPPoll(NewXrayTrafficJob(), poll); err == nil {
				t.Fatal("untracked runtime reported as an empty sample")
			}
			var markers, incidents int64
			if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&incidents).Error; err != nil {
				t.Fatal(err)
			}
			if rejectIncident && (markers != 1 || incidents != 0) || !rejectIncident && (markers != 0 || incidents != 1) {
				t.Fatalf("unexpected shared evidence: marker=%d incident=%d", markers, incidents)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("untracked runtime evidence lost: %v", err)
			}
			if rejectIncident {
				if unlock, err := service.LockResellerAllocation(); err == nil {
					unlock()
					t.Fatal("failed incident persistence released the shared poll fence")
				}
			}
		})
	}
}

func TestL2TPPendingHookWithoutTrackedRuntimeRetainsSharedMarker(t *testing.T) {
	setupIntegrationDB(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	dir := filepath.Join(bin, "l2tp", "hook-incidents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pending.test")
	if err := os.WriteFile(path, []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := &l2tp.Manager{}
	collector := NewXrayTrafficJob()
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		_, _, err := mgr.CollectTrafficChecked()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, errEmptyL2TPSample
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || collector.l2tpHookPollID == "" {
		t.Fatalf("missing shared fence for pending hook without runtime: %v", err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("shared marker did not block allocation")
	}
	var incidents int64
	if err := database.GetDB().Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&incidents).Error; err != nil || incidents != 0 {
		t.Fatalf("transient pending hook created an incident: %d %v", incidents, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := persistL2TPPoll(collector, poll); err != nil || collector.l2tpHookPollID != "" {
		t.Fatalf("completed hook did not permit marker retry: %v", err)
	}
}

func TestL2TPHookIncidentWithoutTrackedRuntimePublishesSharedBarrier(t *testing.T) {
	setupIntegrationDB(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	dir := filepath.Join(bin, "l2tp", "hook-incidents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "up.failed"), []byte("uncertain"), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := &l2tp.Manager{}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		_, _, err := mgr.CollectTrafficChecked()
		return nil, nil, err
	}
	if _, err := persistL2TPPoll(NewXrayTrafficJob(), poll); err == nil {
		t.Fatal("hook incident was silently treated as an empty runtime")
	}
	var incidents int64
	if err := database.GetDB().Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&incidents).Error; err != nil || incidents != 1 {
		t.Fatalf("hook incident did not create a shared barrier: %d %v", incidents, err)
	}
}

func TestL2TPPendingHookRetainsMarkerUntilSuccessfulRetry(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	collector := NewXrayTrafficJob()
	calls := 0
	pending := true
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		calls++
		if pending {
			return nil, nil, &l2tp.HookPendingError{Path: "pending-test"}
		}
		return nil, nil, errEmptyL2TPSample
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || collector.l2tpHookPollID == "" {
		t.Fatalf("pending hook failed to retain marker: %v", err)
	}
	id := collector.l2tpHookPollID
	var markers int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("missing durable marker: %d %v", markers, err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("pending hook allowed allocation")
	}
	// Recovery is allowed independently, but an unresolved foreign marker
	// must prevent this collector from repolling or clearing its own fence.
	if err := db.Create(&model.XrayTrafficInFlight{ID: "foreign-hook-poll"}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || calls != 1 || collector.l2tpHookPollID != id {
		t.Fatalf("foreign marker bypassed: calls=%d id=%q err=%v", calls, collector.l2tpHookPollID, err)
	}
	if err := db.Where("id = ?", "foreign-hook-poll").Delete(&model.XrayTrafficInFlight{}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || calls != 2 || collector.l2tpHookPollID != id {
		t.Fatalf("pending retry changed marker: calls=%d id=%q err=%v", calls, collector.l2tpHookPollID, err)
	}
	pending = false
	if _, err := persistL2TPPoll(collector, poll); err != nil {
		t.Fatalf("completed hook retry failed: %v", err)
	}
	if collector.l2tpHookPollID != "" {
		t.Fatal("successful retry retained marker identity")
	}
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("completed hook marker remained: %d %v", markers, err)
	}
}

func TestL2TPPendingHookRetainsLockUntilUncertainMarkerIsRestored(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	collector := NewXrayTrafficJob()
	pending := true
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		if pending {
			return nil, nil, &l2tp.HookPendingError{Path: "pending-test"}
		}
		return nil, nil, errEmptyL2TPSample
	}
	if _, err := persistL2TPPoll(collector, poll); err == nil || collector.l2tpHookPollID == "" {
		t.Fatalf("pending marker missing: %v", err)
	}
	id := collector.l2tpHookPollID
	for _, sql := range []string{
		"CREATE TRIGGER reject_l2tp_delete BEFORE DELETE ON xray_traffic_in_flights BEGIN SELECT RAISE(ABORT, 'delete failed'); END",
		"CREATE TRIGGER reject_l2tp_restore BEFORE INSERT ON xray_traffic_in_flights BEGIN SELECT RAISE(ABORT, 'restore failed'); END",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
	pending = false
	if _, err := persistL2TPPoll(collector, poll); err == nil || !collector.l2tpRestorePoll || collector.captureUnlock == nil || collector.l2tpHookPollID != id {
		t.Fatalf("uncertain deletion did not retain lock and identity: id=%q restore=%v err=%v", collector.l2tpHookPollID, collector.l2tpRestorePoll, err)
	}
	for _, name := range []string{"reject_l2tp_delete", "reject_l2tp_restore"} {
		if err := db.Exec("DROP TRIGGER " + name).Error; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := persistL2TPPoll(collector, poll); err != nil {
		t.Fatalf("restored marker retry failed: %v", err)
	}
	if collector.l2tpRestorePoll || collector.captureUnlock != nil || collector.l2tpHookPollID != "" {
		t.Fatal("restored poll left its lock or marker state")
	}
	var count int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("restored marker not cleared after retry: %d %v", count, err)
	}
}

func TestL2TPPendingHookMarkerSurvivesCollectorRestart(t *testing.T) {
	setupIntegrationDB(t)
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, nil, &l2tp.HookPendingError{Path: "pending-test"}
	}
	if _, err := persistL2TPPoll(NewXrayTrafficJob(), poll); err == nil {
		t.Fatal("pending hook reported success")
	}
	calls := 0
	if _, err := persistL2TPPoll(NewXrayTrafficJob(), func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		calls++
		return nil, nil, errEmptyL2TPSample
	}); err == nil || calls != 0 {
		t.Fatalf("orphaned marker allowed new poll: calls=%d err=%v", calls, err)
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

func TestL2TPSessionDisappearancePersistsEvidenceAndFencesQuota(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	missing := &l2tp.SessionDisappeared{InboundID: 4, Interface: "ppp0", Email: "alice@example.test", PreviousRx: 100, PreviousTx: 200, Cause: "session absent from sample or interface missing", Additional: []l2tp.SessionObservation{{Interface: "ppp1", Email: "bob@example.test", NoBaseline: true}}}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) { return nil, nil, missing }
	if _, err := persistL2TPPoll(NewXrayTrafficJob(), poll); err == nil {
		t.Fatal("missing session reported success")
	}
	var markerCount, incidents int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markerCount).Error; err != nil || markerCount != 0 {
		t.Fatalf("marker not cleared after durable incident: %d %v", markerCount, err)
	}
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&incidents).Error; err != nil || incidents != 1 {
		t.Fatalf("shared incident missing: %d %v", incidents, err)
	}
	dir := filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-evidence")
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("missing evidence: %v %v", files, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil || !strings.Contains(string(data), missing.Error()) || !strings.Contains(string(data), "bob@example.test\" with no prior sample") {
		t.Fatalf("session evidence not preserved: %q %v", data, err)
	}
	// A separate panel's DB-backed quota check sees the shared incident.
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := (&service.ResellerService{}).CheckTrafficQuota(&model.Reseller{}, 1); err == nil || !strings.Contains(err.Error(), "shared L2TP") {
		t.Fatalf("shared incident did not fence quota: %v", err)
	}
}

func TestL2TPReadFailurePersistsSharedIncident(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	failure := &l2tp.SessionReadFailure{InboundID: 4, Cause: "incomplete marker", Previous: []l2tp.SessionObservation{{Interface: "ppp0", Email: "alice@example.test", PreviousRx: 100, PreviousTx: 200}}}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) { return nil, nil, failure }
	if _, err := persistL2TPPoll(NewXrayTrafficJob(), poll); err == nil {
		t.Fatal("incomplete sample reported success")
	}
	var markers, incidents int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("poll marker not cleared after durable incident: %d %v", markers, err)
	}
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&incidents).Error; err != nil || incidents != 1 {
		t.Fatalf("shared incident missing: %d %v", incidents, err)
	}
	dir := filepath.Join(os.Getenv("XUI_DB_FOLDER"), "l2tp-accounting-evidence")
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("read failure evidence missing: %v %v", files, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if err != nil || !strings.Contains(string(data), failure.Error()) {
		t.Fatalf("read failure baseline not preserved: %q %v", data, err)
	}
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := (&service.ResellerService{}).CheckTrafficQuota(&model.Reseller{}, 1); err == nil || !strings.Contains(err.Error(), "shared L2TP") {
		t.Fatalf("shared incident did not fence quota: %v", err)
	}
}

func TestL2TPReadFailureRetainsMarkerWhenIncidentWriteFails(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_l2tp_read BEFORE INSERT ON reseller_node_counter_incidents BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, nil, &l2tp.SessionReadFailure{InboundID: 4, Cause: "incomplete marker"}
	}
	if _, err := persistL2TPPoll(NewXrayTrafficJob(), poll); err == nil || !strings.Contains(err.Error(), "poll marker") {
		t.Fatalf("failed incident write did not retain marker: %v", err)
	}
	var markers int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("poll marker missing: %d %v", markers, err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("unresolved marker allowed allocation")
	}
}

func TestL2TPSessionDisappearanceRetainsMarkerWhenEvidenceFails(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_l2tp_missing BEFORE INSERT ON reseller_node_counter_incidents BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	missing := &l2tp.SessionDisappeared{InboundID: 4, Interface: "ppp0", Email: "alice", PreviousRx: 10, PreviousTx: 20}
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) { return nil, nil, missing }
	if _, err := persistL2TPPoll(NewXrayTrafficJob(), poll); err == nil || !strings.Contains(err.Error(), "poll marker") {
		t.Fatalf("incident write failure did not retain marker: %v", err)
	}
	var markers int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("poll marker missing: %d %v", markers, err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("unresolved marker allowed allocation")
	}
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
