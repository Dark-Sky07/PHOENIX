package openvpn

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// archiveRetiredCertificateLocked preserves the public certificate of a
// credential that is about to be pruned. It does NOT revoke that certificate;
// a previously issued profile still carries its private key. The archive is
// evidence for operators, not an authorization decision.
//
// certMu must be held. A partial or conflicting archive is retained and blocks
// pruning: evidence is never replaced after a crash or an ambiguous write.
func archiveRetiredCertificateLocked(dir string, cert *x509.Certificate) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("openvpn: open certificate root: %w", err)
	}
	defer root.Close()

	const archive = "retired-certificates"
	info, err := root.Lstat(archive)
	if errors.Is(err, os.ErrNotExist) {
		if err := root.Mkdir(archive, 0o700); err != nil {
			return fmt.Errorf("openvpn: create certificate archive: %w", err)
		}
		// Persist the directory entry before trusting any file inside it.
		parent, openErr := root.Open(".")
		if openErr != nil {
			return openErr
		}
		syncErr := parent.Sync()
		closeErr := parent.Close()
		if syncErr != nil || closeErr != nil {
			return fmt.Errorf("openvpn: sync certificate root: %w", errors.Join(syncErr, closeErr))
		}
		info, err = root.Lstat(archive)
	}
	if err != nil {
		return fmt.Errorf("openvpn: inspect certificate archive: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return errors.New("openvpn: unsafe retired certificate archive; manual review required")
	}

	digest := sha256.Sum256(cert.Raw)
	name := filepath.Join(archive, hex.EncodeToString(digest[:])+".crt")
	payload := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})

	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		before, statErr := root.Lstat(name)
		if statErr != nil {
			return statErr
		}
		if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Mode().Perm()&0o077 != 0 {
			return errors.New("openvpn: unsafe retired certificate evidence")
		}
		file, err = root.OpenFile(name, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		after, statErr := file.Stat()
		data, readErr := io.ReadAll(io.LimitReader(file, int64(len(payload))+1))
		syncErr := file.Sync()
		closeErr := file.Close()
		if statErr != nil || readErr != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("openvpn: inspect retired certificate: %w", errors.Join(statErr, readErr, syncErr, closeErr))
		}
		if !after.Mode().IsRegular() || !os.SameFile(before, after) || !bytes.Equal(data, payload) {
			return errors.New("openvpn: conflicting retired certificate evidence; manual review required")
		}
		// Sync again on retries: a prior run may have failed after writing.
	} else if err != nil {
		return fmt.Errorf("openvpn: create retired certificate evidence: %w", err)
	} else {
		_, writeErr := file.Write(payload)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return fmt.Errorf("openvpn: write retired certificate: %w", errors.Join(writeErr, syncErr, closeErr))
		}
	}

	archiveDir, err := root.Open(archive)
	if err != nil {
		return err
	}
	syncErr := archiveDir.Sync()
	closeErr := archiveDir.Close()
	if syncErr != nil || closeErr != nil {
		return fmt.Errorf("openvpn: sync retired certificate archive: %w", errors.Join(syncErr, closeErr))
	}
	return nil
}
