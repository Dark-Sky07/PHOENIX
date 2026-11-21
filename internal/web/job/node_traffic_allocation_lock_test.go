package job

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
)

func TestNodeSnapshotKeepsAllocationLockThroughMerge(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := withNodeTrafficAllocationLock(func() (bool, error) { close(entered); <-release; return true, nil })
		finished <- err
	}()
	<-entered
	got := make(chan func(), 1)
	go func() {
		unlock, err := service.LockResellerAllocation()
		if err != nil {
			got <- nil
			return
		}
		got <- unlock
	}()
	select {
	case unlock := <-got:
		if unlock != nil {
			unlock()
		}
		close(release)
		t.Fatal("allocation entered before node merge completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	select {
	case unlock := <-got:
		if unlock == nil {
			t.Fatal("lock acquisition failed")
		}
		unlock()
	case <-time.After(3 * time.Second):
		t.Fatal("allocation lock not released")
	}
}

func TestNodeSnapshotFailureReleasesAllocationLock(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "test.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	expected := errors.New("snapshot failed")
	_, err := withNodeTrafficAllocationLock(func() (bool, error) { return false, expected })
	if !errors.Is(err, expected) {
		t.Fatalf("got %v", err)
	}
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}
