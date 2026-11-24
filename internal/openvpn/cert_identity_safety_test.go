package openvpn

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCertificateFilenameCollisionDoesNotLeakProfile(t *testing.T) {
	tempBinFolder(t)
	inst := Instance{Id: 81, Port: 1194}
	first, second := "alice@example.test", "alice_example.test"
	if sanitizeCertName(first) != sanitizeCertName(second) {
		t.Fatal("fixture does not collide")
	}
	original, err := BuildProfile(inst, first, "host.test")
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := clientFiles(dataDirForID(inst.Id), first)
	certBefore, _ := os.ReadFile(certPath)
	keyBefore, _ := os.ReadFile(keyPath)
	if profile, err := BuildProfile(inst, second, "host.test"); err == nil || profile != "" {
		t.Fatalf("collision disclosed profile: %v", err)
	}
	certAfter, _ := os.ReadFile(certPath)
	keyAfter, _ := os.ReadFile(keyPath)
	if string(certBefore) != string(certAfter) || string(keyBefore) != string(keyAfter) {
		t.Fatal("collision altered existing credential")
	}
	again, err := BuildProfile(inst, first, "host.test")
	if err != nil || again != original {
		t.Fatalf("original profile changed: %v", err)
	}
}

func TestCertificateConcurrentCollidingProfiles(t *testing.T) {
	tempBinFolder(t)
	inst := Instance{Id: 82, Port: 1194}
	emails := []string{"alice@example.test", "alice_example.test"}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	results := make([]string, 2)
	for i := range emails {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i], errs[i] = BuildProfile(inst, emails[i], "host.test") }(i)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("exactly one identity should succeed: %v", errs)
	}
	for i := range errs {
		if errs[i] != nil && results[i] != "" {
			t.Fatal("failed request returned credentials")
		}
	}
}

func TestCertificatePreflightKeepsExistingEvidence(t *testing.T) {
	tempBinFolder(t)
	dir := dataDirForID(83)
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := ensureClientCert(dir, "alice@example.test"); err != nil {
		t.Fatal(err)
	}
	crt, key := clientFiles(dir, "alice@example.test")
	data, _ := os.ReadFile(crt)
	certMu.Lock()
	err := validateClientCertSetLocked(dir, []string{"alice@example.test", "alice_example.test"})
	certMu.Unlock()
	if err == nil {
		t.Fatal("alias accepted")
	}
	current := &managed{fingerprint: "old"}
	mgr := &Manager{procs: map[int]*managed{83: current}}
	if err := mgr.Ensure(Instance{Id: 83, Clients: []string{"alice@example.test", "alice_example.test"}}); err == nil {
		t.Fatal("manager accepted colliding certificates")
	}
	if mgr.procs[83] != current {
		t.Fatal("preflight removed the existing managed daemon")
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := ensureClientCert(dir, "alice@example.test"); err == nil {
		t.Fatal("partial material overwritten")
	}
	after, _ := os.ReadFile(crt)
	if string(data) != string(after) {
		t.Fatal("partial certificate changed")
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("key unexpectedly regenerated: %v", err)
	}
}

func TestCertificateMismatchedKeyRejected(t *testing.T) {
	tempBinFolder(t)
	dir := dataDirForID(84)
	for _, email := range []string{"one@example.test", "two@example.test"} {
		if err := ensureClientCert(dir, email); err != nil {
			t.Fatal(err)
		}
	}
	_, key1 := clientFiles(dir, "one@example.test")
	_, key2 := clientFiles(dir, "two@example.test")
	b, err := os.ReadFile(key2)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key1, b, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildProfile(Instance{Id: 84}, "one@example.test", "host"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("mismatched key accepted: %v", err)
	}
	if filepath.Dir(key1) != filepath.Dir(key2) {
		t.Fatal("fixture mismatch")
	}
}
