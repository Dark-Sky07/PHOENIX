package pppvpn

import (
	"testing"
)

func TestManagerDashboardStopControls(t *testing.T) {
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	m := &Manager{inbounds: map[int]*managed{}}

	if err := m.StopManually(); err != nil {
		t.Fatalf("StopManually: %v", err)
	}
	if status := m.Status(); !status.ManualStop || status.Running {
		t.Fatalf("manual stop not reflected in status: %+v", status)
	}

	// A manually stopped group must not be recreated by the reconcile tick,
	// even when the database still reports an enabled inbound.
	m.Reconcile([]Instance{{ID: 1, Protocol: PPTP}})
	m.mu.Lock()
	count := len(m.inbounds)
	m.mu.Unlock()
	if count != 0 {
		t.Fatalf("reconcile recreated daemons after manual stop: %d", count)
	}

	m.Resume()
	if status := m.Status(); status.ManualStop {
		t.Fatalf("Resume did not clear manual stop: %+v", status)
	}
	if err := m.StopManually(); err != nil {
		t.Fatal(err)
	}
	if err := m.Restart(); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if status := m.Status(); status.ManualStop {
		t.Fatalf("Restart left manual stop set: %+v", status)
	}
}
