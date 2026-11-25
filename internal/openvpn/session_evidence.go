package openvpn

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Durable per-session evidence. OpenVPN's status polls cannot see a session
// that starts and ends between two polls. These hook-written records are the
// authoritative source for closed-session usage; the management poll only
// supplies live connection state and inbound counters while a session is open.
// Misuse or loss of evidence is surfaced as a durable incident and never
// converted into guessed bytes or an automatic quota release.
const (
	evidenceRoot    = "session-evidence"
	evidenceRecords = "records"
	evidenceInc     = "incidents"
)

var (
	sessionIDPattern   = regexp.MustCompile(`^[0-9a-fA-F]{16,64}$`)
	generationPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	sessionCommonRegex = regexp.MustCompile(`^[A-Za-z0-9_.+@-]{1,320}$`)
)

func evidencePath(dir string, parts ...string) string {
	return filepath.Join(append([]string{dir, evidenceRoot}, parts...)...)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// newDaemonGeneration is included in hook records so a CID or session value
// reused by a later daemon cannot be mistaken for an earlier session.
func newDaemonGeneration() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// loadOrCreateDaemonGeneration keeps evidence generation stable when a config
// is rendered without a daemon restart (for example by the real parser test).
// A malformed or unsafe file fails closed rather than being regenerated.
func loadOrCreateDaemonGeneration(dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	info, err := root.Lstat("daemon-generation")
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > 128 {
			return "", errors.New("openvpn: unsafe daemon generation file")
		}
		data, err := root.ReadFile("daemon-generation")
		if err != nil {
			return "", err
		}
		generation := strings.TrimSpace(string(data))
		if !generationPattern.MatchString(generation) {
			return "", errors.New("openvpn: malformed daemon generation")
		}
		return generation, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	generation, err := newDaemonGeneration()
	if err != nil {
		return "", err
	}
	if err := writeManagedFile(root, "daemon-generation", []byte(generation+"\n"), 0o600); err != nil {
		return "", err
	}
	return generation, nil
}

// rotateDaemonGeneration is called only when a new daemon process is about to
// start. Evidence from the previous epoch remains on disk with its old value.
func rotateDaemonGeneration(dir string) (string, error) {
	generation, err := newDaemonGeneration()
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := writeManagedFile(root, "daemon-generation", []byte(generation+"\n"), 0o600); err != nil {
		return "", err
	}
	return generation, nil
}

// ensureEvidenceForConfig installs the authorization and hook files that the
// rendered config references. Keeping this with writeConfig prevents any
// caller from producing a config whose scripts are missing.
func ensureEvidenceForConfig(dir string, inst Instance) error {
	generation, err := loadOrCreateDaemonGeneration(dir)
	if err != nil {
		return err
	}
	if err := writeClientAllowlist(dir, inst.Clients); err != nil {
		return err
	}
	return writeSessionEvidenceSetup(dir, inst, generation)
}

func syncDir(root *os.Root, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	err = dir.Sync()
	closeErr := dir.Close()
	return errors.Join(err, closeErr)
}

func stagedName(dir, base string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return filepath.Join(dir, ".tmp."+base+"."+hex.EncodeToString(buf)), nil
}

// writeManagedFile atomically replaces a panel-owned script. Unlike evidence
// files these are reproducible and must be refreshed as configuration changes.
func writeManagedFile(root *os.Root, rel string, data []byte, perm os.FileMode) error {
	parent := filepath.Dir(rel)
	if parent == "." {
		parent = ""
	}
	tmp, err := stagedName(parent, filepath.Base(rel))
	if err != nil {
		return err
	}
	file, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	chmodErr := file.Chmod(perm)
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, chmodErr, closeErr); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if parent != "" {
		return syncDir(root, parent)
	}
	return nil
}

// writeSessionEvidenceSetup installs the authorization verifier and hook
// scripts before the daemon starts. All paths are embedded as shell literals;
// no operator-controlled value is interpreted as shell source.
func writeSessionEvidenceSetup(dir string, inst Instance, generation string) error {
	if !generationPattern.MatchString(generation) {
		return errors.New("openvpn: invalid daemon generation")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range []string{evidenceRoot, filepath.Join(evidenceRoot, evidenceRecords), filepath.Join(evidenceRoot, evidenceInc)} {
		info, err := root.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			if err := root.Mkdir(name, 0o700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("openvpn: unsafe session evidence directory %q", name)
		}
	}
	if err := syncDir(root, "."); err != nil {
		return err
	}

	// OpenVPN does not export a stable session ID to client-connect and
	// client-disconnect scripts (the internal TLS session ID is not in their
	// environment). The connect hook therefore creates a random durable ID and
	// the disconnect hook resolves it by the client identity tuple. A missing
	// or ambiguous admission record fails closed; usage is never guessed.
	connect := `#!/bin/sh
set -eu
umask 077
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
root=` + shellQuote(dir) + `
records="$root/` + evidenceRoot + `/` + evidenceRecords + `"
incidents="$root/` + evidenceRoot + `/` + evidenceInc + `"
incident() {
  f=$(mktemp "$incidents/failure.XXXXXXXXXX") || exit 1
  printf '%s\n' "$1" > "$f" || exit 1
  sync "$f" "$incidents" "$root" || exit 1
  exit 1
}
cn="${common_name:-}"
ip="${trusted_ip:-${trusted_ip6:-}}"
port="${trusted_port:-}"
case "$cn" in ''|*[!A-Za-z0-9_.+@-]*) incident 'invalid OpenVPN client identity' ;; esac
case "$ip" in ''|*[!0-9A-Fa-f:.]*) incident 'invalid OpenVPN client address' ;; esac
case "$port" in ''|*[!0-9]*) incident 'invalid OpenVPN client port' ;; esac
trusted="$ip:$port"
generation=` + shellQuote(generation) + `
for f in "$records"/*.open; do
  [ -f "$f" ] || continue
  grep -Fqx "common_name=$cn" "$f" || continue
  grep -Fqx "trusted=$trusted" "$f" || continue
  grep -Fqx "generation=$generation" "$f" || continue
  incident 'duplicate active client session'
done
sid=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n') || incident 'cannot generate session id'
case "$sid" in ''|*[!0-9a-f]*) incident 'invalid generated session id' ;; esac
tmp=$(mktemp "$records/.connect.XXXXXXXXXX") || incident 'cannot stage session evidence'
printf 'session=%s\ncommon_name=%s\ninbound=%s\ngeneration=%s\ntrusted=%s\n' \
  "$sid" "$cn" ` + strconv.Itoa(inst.Id) + ` "$generation" "$trusted" > "$tmp" || incident 'cannot write session evidence'
sync "$tmp" || incident 'cannot sync session evidence'
ln "$tmp" "$records/$sid.open" 2>/dev/null || incident 'conflicting or duplicate session evidence'
rm -f "$tmp" || incident 'cannot remove staged session evidence'
sync "$records" || incident 'cannot sync session evidence directory'
`

	disconnect := `#!/bin/sh
set -eu
umask 077
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
root=` + shellQuote(dir) + `
records="$root/` + evidenceRoot + `/` + evidenceRecords + `"
incidents="$root/` + evidenceRoot + `/` + evidenceInc + `"
incident() {
  f=$(mktemp "$incidents/failure.XXXXXXXXXX") || exit 1
  printf '%s\n' "$1" > "$f" || exit 1
  sync "$f" "$incidents" "$root" || exit 1
  exit 1
}
cn="${common_name:-}"
ip="${trusted_ip:-${trusted_ip6:-}}"
port="${trusted_port:-}"
rx="${bytes_received:-}"
tx="${bytes_sent:-}"
case "$cn" in ''|*[!A-Za-z0-9_.+@-]*) incident 'invalid disconnect client identity' ;; esac
case "$ip" in ''|*[!0-9A-Fa-f:.]*) incident 'invalid disconnect client address' ;; esac
case "$port" in ''|*[!0-9]*) incident 'invalid disconnect client port' ;; esac
case "$rx" in ''|*[!0-9]*) incident 'invalid disconnect received counter' ;; esac
case "$tx" in ''|*[!0-9]*) incident 'invalid disconnect sent counter' ;; esac
trusted="$ip:$port"
generation=` + shellQuote(generation) + `
sid=''
count=0
for f in "$records"/*.open; do
  [ -f "$f" ] || continue
  grep -Fqx "common_name=$cn" "$f" || continue
  grep -Fqx "trusted=$trusted" "$f" || continue
  grep -Fqx "generation=$generation" "$f" || continue
  count=$((count + 1))
  sid=$(sed -n 's/^session=//p' "$f") || incident 'cannot read admission session id'
done
[ "$count" = 1 ] || incident 'missing or ambiguous admission record'
case "$sid" in ''|*[!0-9a-fA-F]*) incident 'invalid admission session id' ;; esac
tmp=$(mktemp "$records/.disconnect.XXXXXXXXXX") || incident 'cannot stage final evidence'
printf 'session=%s\ncommon_name=%s\nrx=%s\ntx=%s\n' "$sid" "$cn" "$rx" "$tx" > "$tmp" || incident 'cannot write final evidence'
sync "$tmp" || incident 'cannot sync final evidence'
ln "$tmp" "$records/$sid.close" 2>/dev/null || incident 'conflicting or duplicate final evidence'
rm -f "$tmp" || incident 'cannot remove staged final evidence'
sync "$records" || incident 'cannot sync final evidence directory'
`

	// OpenVPN versions differ in the case of tls_digest_sha256_*: 2.7 emits
	// lowercase while older releases emit uppercase. Normalize before the
	// exact, literal-line comparison so the stored uppercase allowlist works
	// on every supported version.
	verifier := `#!/bin/sh
set -u
depth="${1:-0}"
[ "$depth" = 0 ] || exit 0
digest="${tls_digest_sha256_0:-}"
[ -n "$digest" ] || exit 1
case "$digest" in *[!0-9A-Fa-f:]*) exit 1 ;; esac
digest=$(printf '%s' "$digest" | tr '[:lower:]' '[:upper:]') || exit 1
[ -n "$digest" ] || exit 1
allow=` + shellQuote(filepath.Join(dir, "client-allowlist")) + `
[ -f "$allow" ] || exit 1
exec grep -Fqx -- "$digest" "$allow"
`
	scripts := map[string]string{
		"client-connect":    connect,
		"client-disconnect": disconnect,
		"tls-verify":        verifier,
	}
	for name, body := range scripts {
		if err := writeManagedFile(root, name, []byte(body), 0o700); err != nil {
			return fmt.Errorf("openvpn: write %s hook: %w", name, err)
		}
	}
	return nil
}

// certificateDigest formats a certificate digest exactly as OpenVPN exposes
// it to tls-verify: colon-separated uppercase hexadecimal bytes.
func certificateDigest(cert *x509.Certificate) string {
	digest := sha256.Sum256(cert.Raw)
	parts := make([]string, 0, len(digest))
	for _, v := range digest {
		parts = append(parts, fmt.Sprintf("%02X", v))
	}
	return strings.Join(parts, ":")
}

// writeClientAllowlist records the certificate fingerprints accepted for new
// authentications. OpenVPN exposes the client certificate digest as colon
// separated uppercase hex; a missing or unreadable file fails closed in the
// verifier. Existing sessions are not re-authenticated by tls-verify.
func writeClientAllowlist(dir string, clients []string) error {
	revoked, err := loadRevokedDigests(dir)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, email := range clients {
		crt, key := clientFiles(dir, email)
		cert, _, err := loadKeyPair(crt, key)
		if err != nil {
			return fmt.Errorf("openvpn: authorize %q: %w", email, err)
		}
		digest := certificateDigest(cert)
		if revoked[digest] {
			continue
		}
		b.WriteString(digest + "\n")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeManagedFile(root, "client-allowlist", []byte(b.String()), 0o600)
}

// SessionEvidence is one completed session whose final absolute counters
// have been durably recorded by OpenVPN's client-disconnect hook.
type SessionEvidence struct {
	Key       string
	Email     string
	InboundID int
	Rx        int64
	Tx        int64
	Paths     []string
}

// CollectSessionEvidence reads all evidence directories, including those of
// inbounds that are no longer desired. Unknown, incomplete or ambiguous
// evidence fails closed so accounting never silently drops a session.
func CollectSessionEvidence() ([]SessionEvidence, error) {
	if err := CheckSessionEvidenceIncidents(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(openvpnDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, err
	}
	var out []SessionEvidence
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id, err := strconv.Atoi(entry.Name())
		if err != nil || id <= 0 {
			continue
		}
		sessions, err := collectEvidenceForInbound(root, entry.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, sessions...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func collectEvidenceForInbound(root *os.Root, inbound string) ([]SessionEvidence, error) {
	inboundID, err := strconv.Atoi(inbound)
	if err != nil || inboundID <= 0 {
		return nil, fmt.Errorf("openvpn: invalid evidence directory %q", inbound)
	}
	records := filepath.Join(inbound, evidenceRoot, evidenceRecords)
	entries, err := fs.ReadDir(root.FS(), records)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	type recorded struct {
		open, close         string
		openData, closeData []byte
	}
	byID := make(map[string]*recorded)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("openvpn: unsafe session evidence entry %q", name)
		}
		if strings.HasPrefix(name, ".tmp.") || strings.HasPrefix(name, ".connect.") || strings.HasPrefix(name, ".disconnect.") {
			return nil, fmt.Errorf("openvpn: unfinished session hook file %q requires reconciliation", name)
		}
		session, kind, ok := strings.Cut(name, ".")
		if !ok || !sessionIDPattern.MatchString(session) || (kind != "open" && kind != "close") {
			return nil, fmt.Errorf("openvpn: unexpected session evidence file %q", name)
		}
		r := byID[session]
		if r == nil {
			r = &recorded{}
			byID[session] = r
		}
		data, err := root.ReadFile(filepath.Join(records, name))
		if err != nil {
			return nil, err
		}
		if kind == "open" {
			if r.open != "" {
				return nil, fmt.Errorf("openvpn: duplicate session start %q", session)
			}
			r.open, r.openData = name, data
		} else {
			if r.close != "" {
				return nil, fmt.Errorf("openvpn: duplicate session end %q", session)
			}
			r.close, r.closeData = name, data
		}
	}
	var out []SessionEvidence
	for session, r := range byID {
		if r.close == "" {
			// An open record is not proof of usage or of a completed session.
			continue
		}
		if r.open == "" {
			return nil, fmt.Errorf("openvpn: final evidence %q has no admission record", session)
		}
		start, err := parseSessionFields(r.openData)
		if err != nil {
			return nil, fmt.Errorf("openvpn: admission evidence %q: %w", session, err)
		}
		end, err := parseSessionFields(r.closeData)
		if err != nil {
			return nil, fmt.Errorf("openvpn: final evidence %q: %w", session, err)
		}
		if start["session"] != session || end["session"] != session ||
			!sessionCommonRegex.MatchString(start["common_name"]) || start["common_name"] != end["common_name"] ||
			!generationPattern.MatchString(start["generation"]) || start["inbound"] != inbound {
			return nil, fmt.Errorf("openvpn: session identity mismatch %q requires manual review", session)
		}
		rx, err := parseEvidenceCounter(end["rx"])
		if err != nil {
			return nil, fmt.Errorf("openvpn: session %q received counter: %w", session, err)
		}
		tx, err := parseEvidenceCounter(end["tx"])
		if err != nil {
			return nil, fmt.Errorf("openvpn: session %q sent counter: %w", session, err)
		}
		out = append(out, SessionEvidence{
			Key:       "openvpn:" + inbound + ":" + start["generation"] + ":" + session,
			Email:     start["common_name"],
			InboundID: inboundID,
			Rx:        rx,
			Tx:        tx,
			Paths:     []string{filepath.Join(records, r.open), filepath.Join(records, r.close)},
		})
	}
	return out, nil
}

func parseSessionFields(data []byte) (map[string]string, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.Contains(value, "\n") {
			return nil, errors.New("malformed session evidence")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate session evidence field")
		}
		fields[key] = value
	}
	return fields, nil
}

func parseEvidenceCounter(raw string) (int64, error) {
	if raw == "" || strings.Trim(raw, "0123456789") != "" {
		return 0, errors.New("invalid counter")
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value > uint64(maxManagementCounter) {
		return 0, errors.New("counter exceeds supported range")
	}
	return int64(value), nil
}

// CheckSessionEvidenceIncidents reports any hook failure. Presence of an
// incident is never a measurement and never authorizes quota release.
func CheckSessionEvidenceIncidents() error {
	root, err := os.OpenRoot(openvpnDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(entry.Name(), evidenceRoot, evidenceInc)
		files, err := fs.ReadDir(root.FS(), dir)
		if errors.Is(err, os.ErrNotExist) {
			files = nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if len(files) > 0 {
			return fmt.Errorf("openvpn: %d unresolved session evidence incident(s) for inbound %s require manual review", len(files), entry.Name())
		}
		// A configured daemon whose authorization files disappeared must be
		// visible to operators even though tls-verify itself fails closed.
		conf := filepath.Join(entry.Name(), "openvpn.conf")
		if _, err := root.Lstat(conf); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		for _, required := range []string{"client-allowlist", "tls-verify", "client-connect", "client-disconnect"} {
			path := filepath.Join(entry.Name(), required)
			info, err := root.Lstat(path)
			if err != nil {
				return fmt.Errorf("openvpn: inbound %s missing %s; manual review required", entry.Name(), required)
			}
			if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("openvpn: inbound %s has unsafe %s; manual review required", entry.Name(), required)
			}
		}
		if _, err := loadRevokedDigests(filepath.Join(openvpnDir(), entry.Name())); err != nil {
			return fmt.Errorf("openvpn: inbound %s revocation state: %w", entry.Name(), err)
		}
	}
	return nil
}

// PendingSessionEvidence counts admitted sessions whose final counters have
// not been published. These are not automatically billed: only a durable final
// observation can establish usage, and a missing tail requires manual review.
func PendingSessionEvidence() (int, error) {
	root, err := os.OpenRoot(openvpnDir())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		records := filepath.Join(entry.Name(), evidenceRoot, evidenceRecords)
		files, err := fs.ReadDir(root.FS(), records)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".open") {
				continue
			}
			session := strings.TrimSuffix(file.Name(), ".open")
			if _, err := root.Lstat(filepath.Join(records, session+".close")); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return 0, err
			}
			count++
		}
	}
	return count, nil
}

// AckSessionEvidence removes a fully committed session record only after the
// accounting transaction is known to have succeeded. Replays after a crash
// between commit and acknowledgement are idempotent at the database checkpoint.
func AckSessionEvidence(sample SessionEvidence) error {
	root, err := os.OpenRoot(openvpnDir())
	if err != nil {
		return err
	}
	defer root.Close()
	for _, path := range sample.Paths {
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("openvpn: unexpected evidence file %q", path)
		}
		if err := root.Remove(path); err != nil {
			return err
		}
	}
	return syncDir(root, filepath.Dir(sample.Paths[0]))
}

// CheckClientSessionEvidence fences quota-sensitive removal for the given
// accounting identities. Any unresolved hook incident blocks all such
// operations; an admitted session without final counters blocks its owner.
// Neither condition is itself a measurement, and neither is cleared merely by
// restarting the panel.
func CheckClientSessionEvidence(emails []string) error {
	if err := CheckSessionEvidenceIncidents(); err != nil {
		return err
	}
	wanted := make(map[string]bool, len(emails))
	for _, email := range emails {
		key := strings.ToLower(strings.TrimSpace(email))
		if key != "" {
			wanted[key] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	root, err := os.OpenRoot(openvpnDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		records := filepath.Join(entry.Name(), evidenceRoot, evidenceRecords)
		files, err := fs.ReadDir(root.FS(), records)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".open") {
				continue
			}
			session := strings.TrimSuffix(file.Name(), ".open")
			if _, err := root.Lstat(filepath.Join(records, session+".close")); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			data, err := root.ReadFile(filepath.Join(records, file.Name()))
			if err != nil {
				return err
			}
			fields, err := parseSessionFields(data)
			if err != nil {
				return fmt.Errorf("openvpn: unreadable session evidence %q", session)
			}
			if wanted[strings.ToLower(strings.TrimSpace(fields["common_name"]))] {
				return fmt.Errorf("openvpn: client %q has a session without final usage evidence; reconcile before removal", fields["common_name"])
			}
		}
	}
	return nil
}

// ActiveSessionEmails returns CNs whose admission record is not yet paired
// with final evidence. Management deltas for those CNs are deliberately
// suppressed: their complete usage must come from the durable final counter.
func ActiveSessionEmails() map[string]bool {
	out := make(map[string]bool)
	root, err := os.OpenRoot(openvpnDir())
	if err != nil {
		return out
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		records := filepath.Join(entry.Name(), evidenceRoot, evidenceRecords)
		files, err := fs.ReadDir(root.FS(), records)
		if err != nil {
			continue
		}
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".open") {
				continue
			}
			session := strings.TrimSuffix(file.Name(), ".open")
			closed := filepath.Join(records, session+".close")
			if _, err := root.Lstat(closed); err == nil {
				continue
			}
			data, err := root.ReadFile(filepath.Join(records, file.Name()))
			if err != nil {
				continue
			}
			fields, err := parseSessionFields(data)
			if err == nil && sessionCommonRegex.MatchString(fields["common_name"]) {
				out[fields["common_name"]] = true
			}
		}
	}
	return out
}
