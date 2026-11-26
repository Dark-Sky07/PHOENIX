package job

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"sync/atomic"
	"testing"
)

func TestXrayTrafficJobSkipsOverlappingPollUntilPersistenceCompletes(t *testing.T) {
	j := NewXrayTrafficJob()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		defer close(done)
		j.runOnce(func() {
			calls.Add(1)
			close(started)
			<-release // simulate a poll blocked on persistence
		})
	}()
	<-started
	j.runOnce(func() { calls.Add(1) })
	if got := calls.Load(); got != 1 {
		t.Fatalf("overlapping poll ran: got %d calls", got)
	}
	close(release)
	<-done
	j.runOnce(func() { calls.Add(1) })
	if got := calls.Load(); got != 2 {
		t.Fatalf("later poll did not run: got %d calls", got)
	}
}

func TestXrayPollKeepsAllocationLockUntilIdentityCapture(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	j := NewXrayTrafficJob()
	pollCount := 0
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		pollCount++
		return nil, []*xray.ClientTraffic{{Email: "capture@example.test", Up: 12}}, nil
	}
	err := j.prepareXrayBatch(poll, func([]*xray.ClientTraffic) ([]service.XrayTrafficIdentity, error) {
		return nil, errors.New("capture failure")
	})
	if err == nil || j.pending == nil || j.captureUnlock == nil {
		t.Fatalf("failed capture did not retain batch and lock: %v", err)
	}
	gotLock := make(chan func(), 1)
	go func() {
		unlock, err := service.LockResellerAllocation()
		if err != nil {
			gotLock <- nil
			return
		}
		gotLock <- unlock
	}()
	select {
	case unlock := <-gotLock:
		if unlock != nil {
			unlock()
		}
		t.Fatal("mutation acquired lock before capture")
	case <-time.After(100 * time.Millisecond):
	}
	err = j.prepareXrayBatch(poll, func(rows []*xray.ClientTraffic) ([]service.XrayTrafficIdentity, error) {
		return []service.XrayTrafficIdentity{{Email: rows[0].Email, ClientIDs: []int{1}}}, nil
	})
	if err != nil || pollCount != 1 || j.pending.identities == nil || j.captureUnlock == nil {
		t.Fatalf("capture retry = %v, poll count = %d", err, pollCount)
	}
	if _, _, err := j.applyPending(func(batch *xrayTrafficBatch) (bool, bool, error) {
		if err := os.Remove(filepath.Join(xrayJournalDir(), batch.id+".json")); err != nil {
			return false, false, err
		}
		return false, false, service.FinishXrayPoll(batch.id)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-gotLock:
		if unlock == nil {
			t.Fatal("lock acquisition failed")
		}
		unlock()
	case <-time.After(3 * time.Second):
		t.Fatal("mutation remained blocked after application")
	}
}

func TestXrayPollFailureReleasesAllocationLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	j := NewXrayTrafficJob()
	err := j.prepareXrayBatch(func() ([]*xray.Traffic, []*xray.ClientTraffic, error) { return nil, nil, errors.New("poll failed") }, func([]*xray.ClientTraffic) ([]service.XrayTrafficIdentity, error) {
		t.Fatal("capture after failed poll")
		return nil, nil
	})
	if err == nil || j.pending != nil || j.captureUnlock != nil {
		t.Fatalf("poll failure leaked pending state: %v", err)
	}
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestSharedXrayTrafficJobRetainsUncapturedBatchAcrossSchedulerReplacement(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	first := SharedXrayTrafficJob()
	if first.pending != nil || first.captureUnlock != nil {
		t.Fatal("shared job is not clean")
	}
	t.Cleanup(func() {
		if first.captureUnlock != nil {
			first.captureUnlock()
			first.captureUnlock = nil
		}
		first.pending = nil
	})
	polls := 0
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		polls++
		return nil, []*xray.ClientTraffic{{Email: "restart@example.test", Up: 31}}, nil
	}
	if err := first.prepareXrayBatch(poll, func([]*xray.ClientTraffic) ([]service.XrayTrafficIdentity, error) {
		return nil, errors.New("database unavailable")
	}); err == nil {
		t.Fatal("expected capture failure")
	}
	replacement := SharedXrayTrafficJob()
	if replacement != first || replacement.pending == nil || replacement.captureUnlock == nil {
		t.Fatal("scheduler replacement abandoned pending traffic or lock")
	}
	if err := replacement.prepareXrayBatch(poll, func(rows []*xray.ClientTraffic) ([]service.XrayTrafficIdentity, error) {
		return []service.XrayTrafficIdentity{{Email: rows[0].Email}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if polls != 1 || replacement.pending.clients[0].Up != 31 || replacement.captureUnlock == nil {
		t.Fatalf("batch changed or repolled: polls=%d batch=%+v", polls, replacement.pending)
	}
}

func TestXrayPollKeepsAllocationLockUntilJournalIsDurable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	j := NewXrayTrafficJob()
	t.Cleanup(func() {
		if j.captureUnlock != nil {
			j.captureUnlock()
			j.captureUnlock = nil
		}
	})
	polls := 0
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		polls++
		return nil, []*xray.ClientTraffic{{Email: "journal-lock@example.test", Up: 31}}, nil
	}
	captures := 0
	capture := func(rows []*xray.ClientTraffic) ([]service.XrayTrafficIdentity, error) {
		captures++
		// Inject the storage failure after allocation lock acquisition and poll.
		// Using a file instead of a directory also works when tests run as root.
		if err := os.WriteFile(xrayJournalDir(), []byte("not a directory"), 0600); err != nil {
			return nil, err
		}
		return []service.XrayTrafficIdentity{{Email: rows[0].Email}}, nil
	}
	if err := j.prepareXrayBatch(poll, capture); err == nil || j.pending == nil || j.captureUnlock == nil || j.pending.identities == nil {
		t.Fatalf("journal failure did not retain captured batch and lock: %v", err)
	}
	gotLock := make(chan func(), 1)
	go func() {
		unlock, err := service.LockResellerAllocation()
		if err != nil {
			gotLock <- nil
			return
		}
		gotLock <- unlock
	}()
	select {
	case unlock := <-gotLock:
		if unlock != nil {
			unlock()
		}
		t.Fatal("allocation proceeded before durable journal")
	case <-time.After(100 * time.Millisecond):
	}
	if err := os.Remove(xrayJournalDir()); err != nil {
		t.Fatal(err)
	}
	if err := j.prepareXrayBatch(poll, capture); err != nil {
		t.Fatal(err)
	}
	if polls != 1 || captures != 1 || j.captureUnlock == nil {
		t.Fatalf("retry repolled or lost snapshot: polls=%d captures=%d", polls, captures)
	}
	if _, err := os.Stat(filepath.Join(xrayJournalDir(), j.pending.id+".json")); err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	if _, _, err := j.applyPending(func(batch *xrayTrafficBatch) (bool, bool, error) {
		if err := os.Remove(filepath.Join(xrayJournalDir(), batch.id+".json")); err != nil {
			return false, false, err
		}
		return false, false, service.FinishXrayPoll(batch.id)
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-gotLock:
		if unlock == nil {
			t.Fatal("lock acquisition failed")
		}
		unlock()
	case <-time.After(3 * time.Second):
		t.Fatal("allocation lock not released after durable journal")
	}
}

func TestJournaledXrayBatchKeepsAllocationLockedUntilDatabaseCommit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	client := model.ClientRecord{Email: "pending-quota@example.test", Enable: true}
	counter := xray.ClientTraffic{Email: client.Email, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&counter).Error; err != nil {
		t.Fatal(err)
	}
	j := NewXrayTrafficJob()
	t.Cleanup(func() {
		if j.captureUnlock != nil {
			j.captureUnlock()
			j.captureUnlock = nil
		}
	})
	poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
		return nil, []*xray.ClientTraffic{{Email: client.Email, Up: 123}}, nil
	}
	if err := j.prepareXrayBatch(poll, j.inboundService.CaptureXrayTrafficIdentities); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_pending_xray BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.applyPending(j.applyJournaledXrayBatch); err == nil || j.pending == nil || j.captureUnlock == nil {
		t.Fatalf("failed application lost batch or allocation lock: %v", err)
	}
	gotLock := make(chan func(), 1)
	go func() {
		unlock, err := service.LockResellerAllocation()
		if err != nil {
			gotLock <- nil
			return
		}
		gotLock <- unlock
	}()
	select {
	case unlock := <-gotLock:
		if unlock != nil {
			unlock()
		}
		t.Fatal("allocation proceeded with unapplied journal")
	case <-time.After(100 * time.Millisecond):
	}
	if err := db.Exec("DROP TRIGGER reject_pending_xray").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.applyPending(j.applyJournaledXrayBatch); err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-gotLock:
		if unlock == nil {
			t.Fatal("allocation lock acquisition failed")
		}
		defer unlock()
	case <-time.After(3 * time.Second):
		t.Fatal("allocation remained locked after commit")
	}
	if err := db.First(&counter, counter.Id).Error; err != nil || counter.Up != 123 {
		t.Fatalf("allocation resumed without committed usage: %+v %v", counter, err)
	}
}

func TestCounterIncidentRetainsCrossHostPollBarrier(t *testing.T) {
	for _, pollErr := range []error{
		&xray.UserTrafficCounterMissing{Email: "alice@example.test", Direction: "uplink", Previous: 10},
		&xray.UserTrafficCounterDecrease{Email: "alice@example.test", Direction: "downlink", Previous: 10, Observed: 1},
	} {
		t.Run(fmt.Sprintf("%T", pollErr), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XUI_DB_FOLDER", dir)
			if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.CloseDB() })
			j := NewXrayTrafficJob()
			polls := 0
			poll := func() ([]*xray.Traffic, []*xray.ClientTraffic, error) {
				polls++
				return nil, nil, fmt.Errorf("injected incident persistence failure: %w", pollErr)
			}
			capture := func([]*xray.ClientTraffic) ([]service.XrayTrafficIdentity, error) {
				t.Fatal("capture after failed poll")
				return nil, nil
			}
			if err := j.prepareXrayBatch(poll, capture); err == nil {
				t.Fatal("counter incident accepted")
			}
			var markers []model.XrayTrafficInFlight
			if err := database.GetDB().Find(&markers).Error; err != nil || len(markers) != 1 {
				t.Fatalf("cross-host marker missing: %+v %v", markers, err)
			}
			if unlock, err := service.LockResellerAllocation(); err == nil {
				unlock()
				t.Fatal("incident marker failed to block allocations")
			}
			if err := j.prepareXrayBatch(poll, capture); err == nil || polls != 1 {
				t.Fatalf("new poll bypassed barrier: %d %v", polls, err)
			}
			if err := service.RecoverCommittedXrayPollMarkers(); err != nil {
				t.Fatal(err)
			}
			var remaining int64
			if err := database.GetDB().Model(&model.XrayTrafficInFlight{}).Count(&remaining).Error; err != nil || remaining != 1 {
				t.Fatalf("uncommitted marker cleared: %d %v", remaining, err)
			}
		})
	}
}
