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
		name string
		stop func(*Manager, int)
	}{
		{"reconcile", func(m *Manager, _ int) { m.Reconcile(nil) }},
		{"manual stop", func(m *Manager, _ int) { m.StopManually() }},
		{"manual stop reconcile", func(m *Manager, _ int) { m.manualStop = true; m.Reconcile(nil) }},
		{"restart", func(m *Manager, _ int) { m.Restart() }},
		{"stop inbound", func(m *Manager, id int) { m.StopInbound(id) }},
		{"remove", func(m *Manager, id int) { m.Remove(id) }},
		{"stop all", func(m *Manager, _ int) { m.StopAll() }},
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
				tc.stop(m, id)
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
