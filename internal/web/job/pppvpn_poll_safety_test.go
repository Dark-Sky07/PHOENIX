package job

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/pppvpn"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestPPPPollRetriesAmbiguousCommitAndHoldsAllocation(t *testing.T) {
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
	apply := func(rows []*xray.ClientTraffic) (bool, bool, error) {
		calls++
		restart, disabled, err := svc.AddPPPTrafficUnderAllocationLock(rows)
		if err != nil {
			return restart, disabled, err
		}
		if calls == 1 {
			return restart, disabled, errors.New("injected lost commit response")
		}
		return restart, disabled, nil
	}
	ack := func(uint64) error { acks++; return nil }
	if _, _, err := collectAndCommitPPP(state, collect, apply, ack); err == nil || state.unlock == nil || state.marker == "" {
		t.Fatalf("ambiguous commit not retained: %v", err)
	}
	var count int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("marker missing: %d %v", count, err)
	}
	gotLock := make(chan func(), 1)
	go func() {
		unlock, err := service.LockResellerAllocation()
		if err != nil {
			gotLock <- nil
		} else {
			gotLock <- unlock
		}
	}()
	select {
	case unlock := <-gotLock:
		if unlock != nil {
			unlock()
		}
		t.Fatal("allocation passed before retry")
	case <-time.After(100 * time.Millisecond):
	}
	if _, _, err := collectAndCommitPPP(state, collect, apply, ack); err != nil {
		t.Fatal(err)
	}
	if state.unlock != nil || calls != 2 || acks != 1 {
		t.Fatalf("retry state: calls=%d acks=%d", calls, acks)
	}
	select {
	case unlock := <-gotLock:
		if unlock == nil {
			t.Fatal("allocation did not recover")
		}
		unlock()
	case <-time.After(3 * time.Second):
		t.Fatal("allocation lock leaked")
	}
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
	if _, _, err := collectAndCommitPPP(state, collect, nil, nil); err == nil || state.unlock == nil {
		t.Fatalf("scan failure not retained: %v", err)
	}
	// Simulate lost process memory. A process exit releases its advisory lock,
	// but no receipt exists to establish what happened during the poll.
	state.unlock()
	state.unlock = nil
	state.marker = ""
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("orphan marker did not block allocations")
	}
}
