package job

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
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
	if err != nil || pollCount != 1 || j.pending.identities == nil || j.captureUnlock != nil {
		t.Fatalf("capture retry = %v, poll count = %d", err, pollCount)
	}
	select {
	case unlock := <-gotLock:
		if unlock == nil {
			t.Fatal("lock acquisition failed")
		}
		unlock()
	case <-time.After(3 * time.Second):
		t.Fatal("mutation remained blocked after capture")
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
