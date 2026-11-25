package openvpn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var certificateDigestPattern = regexp.MustCompile(`^(?:[0-9A-F]{2}:){31}[0-9A-F]{2}$`)

// revocationsPath is the per-inbound durable deny list. It is independent of
// the generated allowlist so a daemon restart or a re-attached client cannot
// silently authorize a fingerprint that an administrator revoked.
func revocationsPath(dir string) string { return filepath.Join(dir, "client-revocations") }

func loadRevokedDigests(dir string) (map[string]bool, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Lstat("client-revocations")
	if errors.Is(err, os.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > 64*1024 {
		return nil, errors.New("openvpn: unsafe revocation state; manual review required")
	}
	data, err := root.ReadFile("client-revocations")
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		if !certificateDigestPattern.MatchString(line) {
			return nil, errors.New("openvpn: malformed revocation digest; manual review required")
		}
		if out[line] {
			return nil, errors.New("openvpn: duplicate revocation digest; manual review required")
		}
		out[line] = true
	}
	return out, nil
}

// recordRevokedDigest persists the deny decision before any allowlist rewrite
// or session kill. A conflicting or unreadable state file fails closed.
func recordRevokedDigest(dir, digest string) error {
	if !certificateDigestPattern.MatchString(digest) {
		return errors.New("openvpn: invalid certificate digest for revocation")
	}
	existing, err := loadRevokedDigests(dir)
	if err != nil {
		return err
	}
	if existing[digest] {
		return nil
	}
	existing[digest] = true
	digests := make([]string, 0, len(existing))
	for value := range existing {
		digests = append(digests, value)
	}
	sort.Strings(digests)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeManagedFile(root, "client-revocations", []byte(strings.Join(digests, "\n")+"\n"), 0o600)
}

// certificateDigestForClient resolves the currently stored certificate for an
// email and returns its OpenVPN-formatted SHA-256 digest.
func certificateDigestForClient(dir, email string) (string, error) {
	crt, key := clientFiles(dir, email)
	cert, _, err := loadKeyPair(crt, key)
	if err != nil {
		return "", fmt.Errorf("openvpn: resolve client certificate for revocation: %w", err)
	}
	return certificateDigest(cert), nil
}
