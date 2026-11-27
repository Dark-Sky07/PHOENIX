package service

import (
	"os"
	"testing"
)

func TestUpdateMaintenanceMarkerFailsClosedUntilRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if UpdateMaintenanceActive() {
		t.Fatal("missing update marker reported active")
	}
	if err := os.WriteFile(updateMaintenanceMarkerPath(), []byte("pid=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !UpdateMaintenanceActive() {
		t.Fatal("update marker did not block accounting workers")
	}
	if err := os.Remove(updateMaintenanceMarkerPath()); err != nil {
		t.Fatal(err)
	}
	if UpdateMaintenanceActive() {
		t.Fatal("removed update marker remained active")
	}
}
