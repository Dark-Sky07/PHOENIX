package openvpn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReconcileKeepsEarlierInboundError(t *testing.T) {
	tempBinFolder(t)
	// Install an in-memory firewall without invoking host commands.
	original := GetNetworkManager()
	networkMgr = newTestNetworkManager(newFakeFirewall())
	t.Cleanup(func() { networkMgr = original })

	bad := Instance{Id: 86, Port: 1186}
	good := Instance{Id: 87, Port: 1187}
	if err := os.MkdirAll(dataDirForID(bad.Id), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDirForID(bad.Id), "ca.crt"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDirForID(good.Id), 0750); err != nil {
		t.Fatal(err)
	}
	if err := ensureServerCert(dataDirForID(good.Id)); err != nil {
		t.Fatal(err)
	}
	// IsRunning only observes this process handle; it is never signalled.
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	mgr := &Manager{procs: map[int]*managed{good.Id: {fingerprint: good.fingerprint(), proc: &Process{cmd: &exec.Cmd{Process: process}}}}}
	mgr.Reconcile([]Instance{bad, good})
	if got := mgr.Status().Error; !strings.Contains(got, "inbound 86") {
		t.Fatalf("earlier failure hidden: %q", got)
	}
	mgr.Reconcile([]Instance{good})
	if got := mgr.Status().Error; got != "" {
		t.Fatalf("resolved error not cleared: %q", got)
	}
}

func TestStopFailuresRetainManagedProcessAndRules(t *testing.T) {
	for _, tc := range []struct {
		name         string
		stop         func(*Manager, int) error
		returnsError bool
	}{
		{"reconcile", func(m *Manager, _ int) error { m.Reconcile(nil); return nil }, false},
		{"manual stop", func(m *Manager, _ int) error { return m.StopManually() }, true},
		{"manual stop reconcile", func(m *Manager, _ int) error { m.manualStop = true; m.Reconcile(nil); return nil }, false},
		{"restart", func(m *Manager, _ int) error { return m.Restart() }, true},
		{"stop inbound", func(m *Manager, id int) error { return m.StopInbound(id) }, true},
		{"remove", func(m *Manager, id int) error { return m.Remove(id) }, true},
		{"stop all", func(m *Manager, _ int) error { return m.StopAll() }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tempBinFolder(t)
			original := GetNetworkManager()
			firewall := newTestNetworkManager(newFakeFirewall())
			networkMgr = firewall
			t.Cleanup(func() { networkMgr = original })
			const id = 93
			inst := Instance{Id: id, Port: 1194}
			if err := os.MkdirAll(dataDirForID(id), 0750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(pidPathForID(id), []byte("999999\n"), 0600); err != nil {
				t.Fatal(err)
			}
			self, err := os.FindProcess(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			close(done)
			proc := &Process{configPath: confPathForID(id), cmd: &exec.Cmd{Process: self}, done: done}
			proc.intentionalStop.Store(true)
			current := &managed{proc: proc}
			m := &Manager{procs: map[int]*managed{id: current}}
			firewall.rules[id] = stateForInstance(inst)
			// A second attempt must not silently turn the failed cleanup into success.
			for i := 0; i < 2; i++ {
				err := tc.stop(m, id)
				if tc.returnsError && err == nil {
					t.Fatal("stop failure not returned")
				}
				if m.procs[id] != current || !strings.Contains(m.Status().Error, "inbound 93") {
					t.Fatalf("stop failure hidden: %+v", m.Status())
				}
				if _, ok := firewall.rules[id]; !ok {
					t.Fatal("rules removed despite uncertain stop")
				}
				if _, err := os.Stat(pidPathForID(id)); err != nil {
					t.Fatalf("PID evidence lost: %v", err)
				}
			}
		})
	}
}

func TestStoppedDaemonMarksFinalTrafficUnverified(t *testing.T) {
	for _, tc := range []struct {
		name        string
		port        int
		wantWarning bool
	}{
		{"previously started", 12345, true},
		{"failed before start", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := GetNetworkManager()
			networkMgr = newTestNetworkManager(newFakeFirewall())
			t.Cleanup(func() { networkMgr = original })
			m := &Manager{procs: map[int]*managed{1: {proc: &Process{}, mgmtPort: tc.port}}}
			m.mu.Lock()
			err := m.stopManagedLocked(1)
			m.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			m.Resume()
			got := strings.Contains(m.Status().Error, "final usage is unverified")
			if got != tc.wantWarning {
				t.Fatalf("warning=%v want %v: %+v", got, tc.wantWarning, m.Status())
			}
		})
	}
}

func TestDeadStartedDaemonMarksFinalTrafficUnverified(t *testing.T) {
	m := &Manager{procs: map[int]*managed{1: {proc: &Process{}, mgmtPort: 12345}}}
	if _, _, err := m.CollectTrafficWithErrors(); err != nil {
		t.Fatal(err)
	}
	m.Resume()
	if status := m.Status().Error; !strings.Contains(status, "final usage is unverified") {
		t.Fatalf("daemon exit was not visible: %q", status)
	}
}

func TestRunningDaemonStopMarksFinalTrafficUnverified(t *testing.T) {
	original := GetNetworkManager()
	networkMgr = newTestNetworkManager(newFakeFirewall())
	t.Cleanup(func() { networkMgr = original })
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	proc := &Process{cmd: cmd, done: make(chan struct{}), configPath: filepath.Join(t.TempDir(), "server.conf"), logWriter: &procLogWriter{}}
	go proc.wait(cmd)
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-proc.done })
	m := &Manager{procs: map[int]*managed{1: {proc: proc, mgmtPort: 12345}}}
	m.mu.Lock()
	err := m.stopManagedLocked(1)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if status := m.Status().Error; !strings.Contains(status, "final usage is unverified") {
		t.Fatalf("successful running daemon stop hid uncertainty: %q", status)
	}
}
