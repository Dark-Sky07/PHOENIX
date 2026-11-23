package pppvpn

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// TrafficBatch is a retryable snapshot of absolute PPP session counters. The
// normal traffic writer converts them to deltas and persists its checkpoint in
// the same database transaction as client usage/quota updates.
type TrafficBatch struct {
	Clients          []*xray.ClientTraffic
	ClosedSessionIDs []string
	Token            uint64
	closed           []closedSample
}

type closedSample struct {
	path    string
	session pppSession
}

// HookIncidentError reports a durable warning emitted by the hook. Its removal
// alone is not proof that counters or session ownership were reconciled.
type HookIncidentError struct{ Path string }

func (e *HookIncidentError) Error() string {
	return fmt.Sprintf("pppvpn: hook incident %q requires manual review", e.Path)
}

// HookPendingError may be a live hook or one interrupted before completion.
// It must not be treated as either success or proof of failure.
type HookPendingError struct{ Path string }

func (e *HookPendingError) Error() string {
	return fmt.Sprintf("pppvpn: hook operation pending at %q", e.Path)
}

// CheckHookIncidents observes visible hook warnings without touching them.
func CheckHookIncidents() error {
	paths, err := readSessionPaths()
	if err != nil {
		return err
	}
	var pending string
	for _, path := range paths {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "hook-error.") {
			return &HookIncidentError{Path: path}
		}
		if strings.HasPrefix(name, "hook-pending.") {
			pending = path
		}
	}
	if pending != "" {
		return &HookPendingError{Path: pending}
	}
	return nil
}

type pppSession struct {
	iface     string
	email     string
	rx        uint64
	tx        uint64
	sessionID string
	inboundID int
	owner     string
	ifindex   uint64
	closed    bool
}

type pppTrafficCollector struct {
	mu      sync.Mutex
	pending *TrafficBatch
	token   uint64
}

var trafficCollector pppTrafficCollector

// CollectTraffic reads Linux interface byte counters for authenticated PPP
// sessions registered by pppd's ip-up/ip-down hooks. Absolute counters are
// passed to the DB traffic writer, whose persistent transactional checkpoint
// makes retries and panel restarts idempotent.
func CollectTraffic() (*TrafficBatch, error) {
	trafficCollector.mu.Lock()
	defer trafficCollector.mu.Unlock()
	if err := GetManager().CheckUntrackedRuntime(); err != nil {
		return nil, err
	}
	if err := CheckHookIncidents(); err != nil {
		return nil, err
	}
	if trafficCollector.pending != nil {
		return cloneTrafficBatch(trafficCollector.pending), nil
	}

	files, err := readSessionPaths()
	if err != nil {
		return nil, err
	}
	batch := &TrafficBatch{}
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasPrefix(name, "hook-error.") {
			return nil, &HookIncidentError{Path: path}
		}
		if strings.HasPrefix(name, "hook-pending.") {
			return nil, &HookPendingError{Path: path}
		}
		// Only the lock directory is excluded by readSessionPaths. Unexpected
		// hidden files may contain accounting evidence and must not be ignored.
		// ip-up/down creates <session>.tmp.<pid> before an atomic rename.
		// Never acknowledge a temporary file before the hook can rename it.
		if _, suffix, ok := strings.Cut(name, ".tmp."); ok {
			if _, err := strconv.ParseUint(suffix, 10, 64); err == nil {
				return nil, fmt.Errorf("pppvpn: unfinished session hook file %q", path)
			}
		}
		session, readErr := readPPPSession(path)
		if readErr != nil {
			return nil, fmt.Errorf("pppvpn: cannot read session record %q: %w", path, readErr)
		}
		stagedRoot := filepath.Join(rootDir(), "sessions-staged") + string(os.PathSeparator)
		if strings.HasPrefix(path, stagedRoot) && !session.closed {
			return nil, fmt.Errorf("pppvpn: active session in claim %q requires manual review", path)
		}
		if !session.closed {
			session.rx, session.tx, err = readInterfaceBytes(session.iface, session.ifindex)
			if err != nil {
				return nil, fmt.Errorf("pppvpn: cannot read active interface %q: %w", session.iface, err)
			}
		}
		if session.rx > uint64(^uint64(0)>>1) || session.tx > uint64(^uint64(0)>>1) {
			return nil, fmt.Errorf("pppvpn: interface counters exceed signed byte accounting range for %s", session.iface)
		}
		batch.Clients = append(batch.Clients, &xray.ClientTraffic{
			Email:        session.email,
			Enable:       true,
			PPPSessionID: session.sessionID,
			PPPInboundID: session.inboundID,
			PPPRxBytes:   int64(session.rx),
			PPPTxBytes:   int64(session.tx),
		})
		if session.closed {
			batch.closed = append(batch.closed, closedSample{path: path, session: session})
			batch.ClosedSessionIDs = append(batch.ClosedSessionIDs, session.sessionID)
		}
	}
	trafficCollector.token++
	batch.Token = trafficCollector.token
	trafficCollector.pending = batch
	return cloneTrafficBatch(batch), nil
}

// Unlike filepath.Glob, explicit directory reads report permission and I/O
// errors instead of silently treating an inaccessible spool as empty.
func readSessionPaths() ([]string, error) {
	root, err := os.OpenRoot(rootDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pppvpn: open session root: %w", err)
	}
	defer root.Close()
	groups, err := readPPPDir(root, "sessions")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("pppvpn: read session directory: %w", err)
	}
	var paths []string
	for _, group := range groups {
		if !group.IsDir() {
			return nil, fmt.Errorf("pppvpn: unexpected session group %q", group.Name())
		}
		dir := filepath.Join("sessions", group.Name())
		entries, err := readPPPDir(root, dir)
		if err != nil {
			return nil, fmt.Errorf("pppvpn: read session group %q: %w", group.Name(), err)
		}
		for _, entry := range entries {
			// Hook lock files persist so concurrent hooks never lock different
			// inodes. The private directory is not an accounting spool.
			if entry.Name() == ".hook-locks" {
				if !entry.IsDir() {
					return nil, fmt.Errorf("pppvpn: invalid hook lock directory %q", dir)
				}
				continue
			}
			if entry.IsDir() {
				return nil, fmt.Errorf("pppvpn: unexpected session directory %q", entry.Name())
			}
			paths = append(paths, filepath.Join(rootDir(), dir, entry.Name()))
		}
	}
	// Claimed records are kept outside hook-controlled session directories.
	// A crash or mismatch leaves them visible for a later idempotent poll.
	staged := "sessions-staged"
	claims, err := readPPPDir(root, staged)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("pppvpn: read staged sessions: %w", err)
	}
	for _, claim := range claims {
		if !claim.IsDir() {
			return nil, fmt.Errorf("pppvpn: invalid staged session %q", claim.Name())
		}
		dir := filepath.Join(staged, claim.Name())
		entries, err := readPPPDir(root, dir)
		if err != nil {
			return nil, fmt.Errorf("pppvpn: read session claim: %w", err)
		}
		// An interrupted claim can be empty before a rename, after one, or
		// after deletion. The directory alone cannot distinguish these states
		// or prove that the source and its committed counters survived a crash.
		if len(entries) == 0 {
			return nil, fmt.Errorf("pppvpn: empty session claim %q requires manual reconciliation", dir)
		}
		for _, entry := range entries {
			if entry.Name() != "record" || entry.IsDir() {
				return nil, fmt.Errorf("pppvpn: invalid session claim entry %q", entry.Name())
			}
			paths = append(paths, filepath.Join(rootDir(), dir, entry.Name()))
		}
	}
	return paths, nil
}

// AckTraffic removes closed-session spool files only after the DB transaction
// has committed their final absolute counters and client usage deltas.
func AckTraffic(token uint64) error {
	trafficCollector.mu.Lock()
	defer trafficCollector.mu.Unlock()
	if trafficCollector.pending == nil || trafficCollector.pending.Token != token {
		return errors.New("pppvpn: stale traffic batch acknowledgement")
	}
	var ackErr error
	for _, sample := range trafficCollector.pending.closed {
		if err := acknowledgeClosedSample(sample); err != nil {
			ackErr = errors.Join(ackErr, err)
		}
	}
	// Failed or changed records remain visible for the next idempotent poll.
	trafficCollector.pending = nil
	return ackErr
}

// Move a path into a private claim before comparing it to the committed sample.
// Hooks only write in sessions/<group>, never under sessions-staged. This avoids
// a check-then-unlink race with an atomic hook replacement at the original path.
// The claim is retained on any error, including a different or malformed record.
func acknowledgeClosedSample(sample closedSample) error {
	root, err := os.OpenRoot(rootDir())
	if err != nil {
		return fmt.Errorf("pppvpn: open spool root: %w", err)
	}
	defer root.Close()
	source, err := pppRelativePath(sample.path)
	if err != nil {
		return err
	}
	staged := "sessions-staged"
	if err := root.MkdirAll(staged, 0700); err != nil {
		return fmt.Errorf("pppvpn: create claim directory: %w", err)
	}
	if err := syncPPPDirectory(root, "."); err != nil {
		return err
	}
	var dir string
	for attempt := 0; attempt < 10; attempt++ {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		dir = filepath.Join(staged, "claim-"+hex.EncodeToString(token[:]))
		err = root.Mkdir(dir, 0700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("pppvpn: create session claim: %w", err)
		}
		break
	}
	if err != nil {
		return fmt.Errorf("pppvpn: cannot allocate session claim: %w", err)
	}
	if err := syncPPPDirectory(root, staged); err != nil {
		return err
	}
	claimed := filepath.Join(dir, "record")
	if err := root.Rename(source, claimed); err != nil {
		return fmt.Errorf("pppvpn: claim session record %q: %w", sample.path, err)
	}
	if err := syncPPPDirectory(root, dir); err != nil {
		return err
	}
	if err := syncPPPDirectory(root, filepath.Dir(source)); err != nil {
		return err
	}
	// A recovered claim is itself moved into a fresh claim. Remove its now
	// empty directory only after the destination and source are durable.
	sourceDir := filepath.Dir(source)
	if filepath.Dir(sourceDir) == staged && filepath.Base(source) == "record" {
		if err := root.Remove(sourceDir); err != nil {
			return fmt.Errorf("pppvpn: remove recovered session claim: %w", err)
		}
		if err := syncPPPDirectory(root, staged); err != nil {
			return err
		}
	}
	current, err := readPPPSessionAt(root, claimed)
	if err != nil {
		return fmt.Errorf("pppvpn: inspect claimed session %q: %w", claimed, err)
	}
	if current != sample.session {
		return fmt.Errorf("pppvpn: session changed before acknowledgement; retained %q", claimed)
	}
	if err := root.Remove(claimed); err != nil {
		return fmt.Errorf("pppvpn: remove committed session claim: %w", err)
	}
	if err := syncPPPDirectory(root, dir); err != nil {
		return err
	}
	if err := root.Remove(dir); err != nil {
		return fmt.Errorf("pppvpn: remove empty session claim: %w", err)
	}
	return syncPPPDirectory(root, staged)
}

func pppRelativePath(path string) (string, error) {
	rel, err := filepath.Rel(rootDir(), path)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return "", errors.New("pppvpn: path outside session root")
	}
	return rel, nil
}

func readPPPDir(root *os.Root, name string) ([]os.DirEntry, error) {
	f, err := openPPPDirectory(root, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func syncPPPDirectory(root *os.Root, name string) error {
	f, err := openPPPDirectory(root, name)
	if err != nil {
		return fmt.Errorf("pppvpn: open directory for sync: %w", err)
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("pppvpn: sync directory: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("pppvpn: close directory: %w", closeErr)
	}
	return nil
}

func cloneTrafficBatch(batch *TrafficBatch) *TrafficBatch {
	if batch == nil {
		return nil
	}
	copyBatch := &TrafficBatch{Token: batch.Token, Clients: make([]*xray.ClientTraffic, 0, len(batch.Clients)), ClosedSessionIDs: append([]string(nil), batch.ClosedSessionIDs...), closed: append([]closedSample(nil), batch.closed...)}
	for _, client := range batch.Clients {
		if client != nil {
			copyClient := *client
			copyBatch.Clients = append(copyBatch.Clients, &copyClient)
		}
	}
	return copyBatch
}

func readPPPSession(path string) (pppSession, error) {
	rel, err := pppRelativePath(path)
	if err != nil {
		return pppSession{}, err
	}
	root, err := os.OpenRoot(rootDir())
	if err != nil {
		return pppSession{}, err
	}
	defer root.Close()
	return readPPPSessionAt(root, rel)
}

func readPPPSessionAt(root *os.Root, rel string) (pppSession, error) {
	file, err := openPPPRecord(root, rel)
	if err != nil {
		return pppSession{}, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		if scanner.Err() != nil {
			return pppSession{}, scanner.Err()
		}
		return pppSession{}, errors.New("pppvpn: empty session record")
	}
	line := scanner.Text()
	if scanner.Scan() {
		return pppSession{}, errors.New("pppvpn: extra session record data")
	}
	if err := scanner.Err(); err != nil {
		return pppSession{}, err
	}
	fields := strings.Split(line, "\t")
	if (len(fields) != 7 && len(fields) != 9) || (fields[0] != "active" && fields[0] != "closed") || fields[1] == "" || fields[2] == "" || fields[5] == "" {
		return pppSession{}, errors.New("pppvpn: malformed session record")
	}
	rx, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		return pppSession{}, err
	}
	tx, err := strconv.ParseUint(fields[4], 10, 64)
	if err != nil {
		return pppSession{}, err
	}
	inboundID, err := strconv.Atoi(fields[6])
	if err != nil || inboundID <= 0 {
		return pppSession{}, errors.New("pppvpn: invalid inbound id in session record")
	}
	var owner string
	var ifindex uint64
	if len(fields) == 9 {
		owner = fields[7]
		ifindex, err = strconv.ParseUint(fields[8], 10, 64)
		if err != nil || ifindex == 0 || owner == "" {
			return pppSession{}, errors.New("pppvpn: invalid session owner or interface generation")
		}
	} else if fields[0] == "active" {
		return pppSession{}, errors.New("pppvpn: legacy active session has no interface generation; manual review required")
	}
	return pppSession{iface: fields[1], email: fields[2], rx: rx, tx: tx, sessionID: fields[5], inboundID: inboundID, owner: owner, ifindex: ifindex, closed: fields[0] == "closed"}, nil
}

func readInterfaceBytes(iface string, expected uint64) (uint64, uint64, error) {
	if strings.ContainsAny(iface, "/\\\x00") || iface == "." || iface == ".." {
		return 0, 0, errors.New("pppvpn: invalid interface name")
	}
	dir := filepath.Join("/sys/class/net", iface)
	if expected == 0 {
		return 0, 0, errors.New("pppvpn: missing interface generation")
	}
	index, err := readCounter(filepath.Join(dir, "ifindex"))
	if err != nil {
		return 0, 0, err
	}
	if index != expected {
		return 0, 0, errors.New("pppvpn: interface generation changed")
	}
	base := filepath.Join(dir, "statistics")
	rx, err := readCounter(filepath.Join(base, "rx_bytes"))
	if err != nil {
		return 0, 0, err
	}
	tx, err := readCounter(filepath.Join(base, "tx_bytes"))
	if err != nil {
		return 0, 0, err
	}
	index, err = readCounter(filepath.Join(dir, "ifindex"))
	if err != nil {
		return 0, 0, err
	}
	if index != expected {
		return 0, 0, errors.New("pppvpn: interface generation changed during sampling")
	}
	return rx, tx, nil
}

func readCounter(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}

// CheckClientSessionEvidence blocks destructive accounting operations while a
// matching local spool record exists, whether active, closed or staged. A
// missing record does not prove that no hook, session or unpolled usage exists.
// scope == nil checks all inbound IDs; a non-nil empty scope checks none.
func CheckClientSessionEvidence(emails []string, scope []int) error {
	if scope != nil && len(scope) == 0 {
		return nil
	}
	wanted := make(map[string]bool, len(emails))
	for _, email := range emails {
		if key := strings.ToLower(strings.TrimSpace(email)); key != "" {
			wanted[key] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	if err := GetManager().CheckUntrackedRuntime(); err != nil {
		return err
	}
	if err := CheckHookIncidents(); err != nil {
		return err
	}
	allowed := make(map[int]bool, len(scope))
	for _, id := range scope {
		allowed[id] = true
	}
	paths, err := readSessionPaths()
	if err != nil {
		return err
	}
	for _, path := range paths {
		name := filepath.Base(path)
		// A partially published file can contain usage or identity not yet
		// readable; do not guess whether it belongs to these clients.
		if _, suffix, ok := strings.Cut(name, ".tmp."); ok {
			if _, err := strconv.ParseUint(suffix, 10, 64); err == nil {
				return fmt.Errorf("pppvpn: unfinished session record %q", path)
			}
		}
		session, err := readPPPSession(path)
		if err != nil {
			return fmt.Errorf("pppvpn: inspect session evidence %q: %w", path, err)
		}
		if wanted[strings.ToLower(strings.TrimSpace(session.email))] && (scope == nil || allowed[session.inboundID]) {
			return fmt.Errorf("pppvpn: session evidence for inbound %d requires manual reconciliation before removal", session.inboundID)
		}
	}
	return nil
}
