package controller

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
)

func TestLoginLimiterPersistsBlockAcrossDatabaseRestart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	path := filepath.Join(dir, "x-ui.db")
	if err := database.InitDB(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })

	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first := newLoginLimiter(3, 5*time.Minute, 15*time.Minute)
	first.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		_, blocked := first.registerFailure("198.51.100.8", "Admin")
		if blocked != (i == 2) {
			t.Fatalf("failure %d blocked = %v", i+1, blocked)
		}
	}
	if err := database.CloseDB(); err != nil {
		t.Fatal(err)
	}
	if err := database.InitDB(path); err != nil {
		t.Fatal(err)
	}
	second := newLoginLimiter(3, 5*time.Minute, 15*time.Minute)
	second.now = func() time.Time { return now.Add(time.Minute) }
	until, allowed := second.allow("198.51.100.8", "admin")
	if allowed || !until.After(now) {
		t.Fatalf("restart lost login block: allowed=%v until=%v", allowed, until)
	}
	second.registerSuccess("198.51.100.8", "admin")
	if _, allowed := second.allow("198.51.100.8", "admin"); !allowed {
		t.Fatal("successful login must clear its durable counter")
	}
}
