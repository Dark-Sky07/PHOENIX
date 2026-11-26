package service

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
)

func TestSQLiteAllocationLockHelper(t *testing.T) {
	path := os.Getenv("PHOENIX_SQLITE_LOCK_HELPER")
	if path == "" {
		t.Skip("subprocess helper")
	}
	unlock, acquired, err := acquireSQLiteAllocationFileLock(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		unlock()
	}
	if want := os.Getenv("PHOENIX_SQLITE_LOCK_EXPECT"); (want == "acquired") != acquired {
		t.Fatalf("want %s, acquired=%v", want, acquired)
	}
}

func TestSQLiteAllocationLockCoordinatesProcesses(t *testing.T) {
	setupBulkDB(t)
	path, err := sqliteAllocationDatabasePath(database.GetDB())
	if err != nil || path == "" {
		t.Fatalf("main database path: %q %v", path, err)
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	run := func(expect string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteAllocationLockHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "PHOENIX_SQLITE_LOCK_HELPER="+path, "PHOENIX_SQLITE_LOCK_EXPECT="+expect)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child expecting %s failed: %v\n%s", expect, err, output)
		}
	}
	run("busy")
	unlock()
	unlock = nil
	run("acquired")
}

func TestSQLiteAllocationLockFailsClosedIfSidecarCannotOpen(t *testing.T) {
	path := t.TempDir() + "/db"
	if err := os.Mkdir(path+".reseller-allocation.lock", 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := acquireSQLiteAllocationFileLock(path, false)
	if err == nil || ok || unlock != nil {
		t.Fatalf("expected file error, got lock=%v error=%v", ok, err)
	}
}
