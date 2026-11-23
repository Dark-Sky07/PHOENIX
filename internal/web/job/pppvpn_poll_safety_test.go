package job

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/pppvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestPPPPollRetriesAmbiguousCommitAndFencesAllocation(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	client := model.ClientRecord{Email: "ppp-poll@example.test", Enable: true}
	row := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, item := range []any{&client, &row} {
		if err := db.Create(item).Error; err != nil {
			t.Fatal(err)
		}
	}
	state := &pppPollState{}
	t.Cleanup(func() {
		if state.unlock != nil {
			state.unlock()
			state.unlock = nil
		}
	})
	collect := func() (*pppvpn.TrafficBatch, error) {
		return &pppvpn.TrafficBatch{Token: 1, Clients: []*xray.ClientTraffic{{Email: client.Email, PPPSessionID: "ppp-poll", PPPInboundID: 7, PPPRxBytes: 123, PPPTxBytes: 456}}}, nil
	}
	svc := &service.InboundService{}
	calls, acks := 0, 0
	apply := func(marker string, rows []*xray.ClientTraffic) (bool, bool, error) {
		calls++
		restart, disabled, err := svc.AddPPPTrafficUnderAllocationLock(marker, rows)
		if err != nil {
			return restart, disabled, err
		}
		if calls == 1 {
			return restart, disabled, errors.New("injected lost commit response")
		}
		return restart, disabled, nil
	}
	ack := func(uint64) error { acks++; return nil }
	if _, _, err := collectAndCommitPPP(state, collect, apply, ack); err == nil || state.unlock != nil || state.marker == "" {
		t.Fatalf("ambiguous commit not retained: %v", err)
	}
	var count int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("marker missing: %d %v", count, err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("allocation passed before retry")
	}
	// Other traffic journal recovery is not blocked between PPP attempts.
	recoveryUnlock, err := service.LockResellerAllocationForXrayRecovery()
	if err != nil {
		t.Fatalf("independent recovery blocked: %v", err)
	}
	recoveryUnlock()
	if _, _, err := collectAndCommitPPP(state, collect, apply, ack); err != nil {
		t.Fatal(err)
	}
	if state.unlock != nil || state.marker != "" || calls != 2 || acks != 1 {
		t.Fatalf("retry state: calls=%d acks=%d", calls, acks)
	}
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatalf("allocation did not recover: %v", err)
	}
	unlock()
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 123 || row.Down != 456 {
		t.Fatalf("sample counted incorrectly: %+v %v", row, err)
	}
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker remained: %d %v", count, err)
	}
}

func TestPPPPollCounterDecreasePublishesBeforeUnlock(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	client := model.ClientRecord{Email: "ppp-counter@example.test", Enable: true}
	row := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, item := range []any{&client, &row, &model.PPPClientTrafficCounter{SessionID: "decreased", Email: client.Email, InboundID: 3, RxBytes: 100, TxBytes: 90}} {
		if err := db.Create(item).Error; err != nil {
			t.Fatal(err)
		}
	}
	state := &pppPollState{}
	t.Cleanup(func() {
		if state.unlock != nil {
			state.unlock()
			state.unlock = nil
		}
	})
	collect := func() (*pppvpn.TrafficBatch, error) {
		return &pppvpn.TrafficBatch{Token: 1, Clients: []*xray.ClientTraffic{{Email: client.Email, PPPSessionID: "decreased", PPPInboundID: 3, PPPRxBytes: 10, PPPTxBytes: 90}}}, nil
	}
	ack := func(uint64) error { t.Fatal("uncertain batch acknowledged"); return nil }
	_, _, err := collectAndCommitPPP(state, collect, (&service.InboundService{}).AddPPPTrafficUnderAllocationLock, ack)
	if err == nil || !strings.Contains(err.Error(), "decreased") || state.unlock != nil {
		t.Fatalf("incident not published and unlocked: %v", err)
	}
	var marker, incident int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&marker).Error; err != nil || marker != 0 {
		t.Fatalf("marker remains: %d %v", marker, err)
	}
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -3).Count(&incident).Error; err != nil || incident != 1 {
		t.Fatalf("incident missing: %d %v", incident, err)
	}
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatalf("other polls blocked: %v", err)
	}
	unlock()
}

func TestPPPPollCrashLeavesCrossHostMarker(t *testing.T) {
	setupIntegrationDB(t)
	state := &pppPollState{}
	collect := func() (*pppvpn.TrafficBatch, error) { return nil, errors.New("injected scan failure") }
	if _, _, err := collectAndCommitPPP(state, collect, nil, nil); err == nil || state.unlock != nil || state.marker == "" {
		t.Fatalf("scan failure not retained: %v", err)
	}
	// Simulate lost process memory. A process exit releases its advisory lock,
	// but no receipt exists to establish what happened during the poll.
	state.marker = ""
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("orphan marker did not block allocations")
	}
}

func TestPPPPollRetryRejectsMissingMarker(t *testing.T) {
	setupIntegrationDB(t)
	state := &pppPollState{}
	collect := func() (*pppvpn.TrafficBatch, error) { return nil, errors.New("scan failed") }
	if _, _, err := collectAndCommitPPP(state, collect, nil, nil); err == nil {
		t.Fatal("expected failure")
	}
	if err := service.FinishXrayPoll(state.marker); err != nil {
		t.Fatal(err)
	}
	called := false
	_, _, err := collectAndCommitPPP(state, func() (*pppvpn.TrafficBatch, error) { called = true; return nil, nil }, nil, nil)
	if err == nil || called || state.marker == "" {
		t.Fatalf("missing fence accepted: %v", err)
	}
}

func TestPPPPollRestoresAmbiguouslyDeletedMarkerBeforeUnlock(t *testing.T) {
	setupIntegrationDB(t)
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatal(err)
	}
	state := &pppPollState{unlock: unlock, marker: "uncertain-delete", deleteUncertain: true}
	t.Cleanup(func() {
		if state.unlock != nil {
			state.unlock()
		}
	})
	// Emulate a deletion that committed but whose response was lost.
	collect := func() (*pppvpn.TrafficBatch, error) { return nil, errors.New("injected scan failure") }
	if _, _, err := collectAndCommitPPP(state, collect, nil, nil); err == nil {
		t.Fatal("scan should fail")
	}
	if state.unlock != nil || state.deleteUncertain {
		t.Fatal("retry retained lock or uncertain state")
	}
	if err := service.VerifyXrayPollMarker(state.marker); err != nil {
		t.Fatalf("fence not restored: %v", err)
	}
	if u, err := service.LockResellerAllocation(); err == nil {
		u()
		t.Fatal("allocation bypassed restored marker")
	}
}

func TestPPPPollAckFailureRetainsMarkerUntilRepoll(t *testing.T) {
	setupIntegrationDB(t)
	state := &pppPollState{}
	collects, acks := 0, 0
	collect := func() (*pppvpn.TrafficBatch, error) {
		collects++
		return &pppvpn.TrafficBatch{Token: uint64(collects)}, nil
	}
	ack := func(uint64) error {
		acks++
		if acks == 1 {
			return errors.New("replacement detected")
		}
		return nil
	}
	if _, _, err := collectAndCommitPPP(state, collect, nil, ack); err == nil || state.marker == "" || state.unlock != nil {
		t.Fatalf("ack failure did not retain fence: %v", err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("allocation bypassed uncertain acknowledgement")
	}
	if _, _, err := collectAndCommitPPP(state, collect, nil, ack); err != nil {
		t.Fatal(err)
	}
	if state.marker != "" || collects != 2 || acks != 2 {
		t.Fatalf("retry did not finish: %d %d", collects, acks)
	}
}

func TestPPPPollRecoversHookReplacementAfterCommittedBatch(t *testing.T) {
	setupIntegrationDB(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	dir := filepath.Join(bin, "pppvpn", "sessions", "pptp-7")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ppp0.closed.session")
	if err := os.WriteFile(path, []byte("closed\tppp0\tppp-replace@example.test\t10\t20\tsession\t7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()
	client := model.ClientRecord{Email: "ppp-replace@example.test", Enable: true}
	row := xray.ClientTraffic{Email: client.Email, Enable: true}
	for _, item := range []any{&client, &row} {
		if err := db.Create(item).Error; err != nil {
			t.Fatal(err)
		}
	}
	state := &pppPollState{}
	t.Cleanup(func() {
		if state.unlock != nil {
			state.unlock()
		}
	})
	svc := &service.InboundService{}
	first := true
	apply := func(marker string, clients []*xray.ClientTraffic) (bool, bool, error) {
		restart, disabled, err := svc.AddPPPTrafficUnderAllocationLock(marker, clients)
		if err == nil && first {
			first = false
			tmp := filepath.Join(dir, "replacement")
			if err := os.WriteFile(tmp, []byte("closed\tppp0\tppp-replace@example.test\t30\t40\tsession\t7\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		}
		return restart, disabled, err
	}
	if _, _, err := collectAndCommitPPP(state, pppvpn.CollectTraffic, apply, pppvpn.AckTraffic); err == nil || state.marker == "" {
		t.Fatalf("replacement did not retain marker: %v", err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("allocation bypassed retained replacement")
	}
	if _, _, err := collectAndCommitPPP(state, pppvpn.CollectTraffic, apply, pppvpn.AckTraffic); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 30 || row.Down != 40 {
		t.Fatalf("replacement accounting: %+v, %v", row, err)
	}
	if state.marker != "" {
		t.Fatal("marker not cleared")
	}
}

func TestPPPPollOrphanRequiresReviewEvenIfDirectoryDisappears(t *testing.T) {
	setupIntegrationDB(t)
	state := &pppPollState{}
	calls := 0
	collect := func() (*pppvpn.TrafficBatch, error) {
		calls++
		return nil, &pppvpn.UntrackedRuntimeError{Detail: "test orphan"}
	}
	if _, _, err := collectAndCommitPPP(state, collect, nil, nil); err == nil || !state.requiresReview || state.marker == "" || state.unlock != nil {
		t.Fatalf("orphan not fenced: %v", err)
	}
	if _, _, err := collectAndCommitPPP(state, collect, nil, nil); err == nil || calls != 1 || state.marker == "" {
		t.Fatalf("orphan auto-cleared: %v calls=%d", err, calls)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("allocation bypassed orphan marker")
	}
}

func TestPPPRealCollectorFencesUntrackedRuntime(t *testing.T) {
	setupIntegrationDB(t)
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	orphan := filepath.Join(bin, "pppvpn", "sstp", "888")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	state := &pppPollState{}
	if _, _, err := collectAndCommitPPP(state, pppvpn.CollectTraffic, nil, pppvpn.AckTraffic); err == nil || !state.requiresReview || state.marker == "" {
		t.Fatalf("real orphan not fenced: %v", err)
	}
	if err := os.RemoveAll(orphan); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectAndCommitPPP(state, pppvpn.CollectTraffic, nil, pppvpn.AckTraffic); err == nil {
		t.Fatal("orphan disappearance automatically cleared fence")
	}
	// Simulate process exit: the persistent marker blocks another host.
	state = &pppPollState{}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("orphan marker did not survive lost process state")
	}
}
