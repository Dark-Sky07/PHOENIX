package openvpn

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestRetiredCertificateEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := ensureClientCert(dir, "alice@example.test"); err != nil {
		t.Fatal(err)
	}
	crt, key := clientFiles(dir, "alice@example.test")
	cert, _, err := loadKeyPair(crt, key)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(cert.Raw)
	archived := filepath.Join(dir, "retired-certificates", hex.EncodeToString(digest[:])+".crt")
	if err := pruneClientCerts(dir, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(archived)
	if err != nil || len(got) == 0 {
		t.Fatalf("missing evidence: %v", err)
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatalf("private key retained: %v", err)
	}
	if err := archiveRetiredCertificateLocked(dir, cert); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := os.WriteFile(archived, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := archiveRetiredCertificateLocked(dir, cert); err == nil {
		t.Fatal("accepted conflicting evidence")
	}
}

func TestPruneRejectsIncompletePair(t *testing.T) {
	dir := t.TempDir()
	if err := ensureClientCert(dir, "alice@example.test"); err != nil {
		t.Fatal(err)
	}
	crt, key := clientFiles(dir, "alice@example.test")
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := pruneClientCerts(dir, nil); err == nil {
		t.Fatal("accepted incomplete pair")
	}
	if _, err := os.Stat(crt); err != nil {
		t.Fatalf("lost certificate: %v", err)
	}
}
