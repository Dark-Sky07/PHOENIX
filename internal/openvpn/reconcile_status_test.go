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
