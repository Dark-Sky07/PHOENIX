package pppvpn

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

type pppSession struct {
	iface     string
	email     string
	rx        uint64
	tx        uint64
	sessionID string
	inboundID int
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
		if strings.HasPrefix(name, ".") {
			continue
		}
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
			session.rx, session.tx, err = readInterfaceBytes(session.iface)
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
	root := filepath.Join(rootDir(), "sessions")
	groups, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("pppvpn: read session directory: %w", err)
	}
	var paths []string
	for _, group := range groups {
		if !group.IsDir() {
			return nil, fmt.Errorf("pppvpn: unexpected session group %q", group.Name())
		}
		dir := filepath.Join(root, group.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("pppvpn: read session group %q: %w", group.Name(), err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				return nil, fmt.Errorf("pppvpn: unexpected session directory %q", entry.Name())
			}
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	// Claimed records are kept outside hook-controlled session directories.
	// A crash or mismatch leaves them visible for a later idempotent poll.
	staged := filepath.Join(rootDir(), "sessions-staged")
	claims, err := os.ReadDir(staged)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("pppvpn: read staged sessions: %w", err)
	}
	for _, claim := range claims {
		if !claim.IsDir() {
			return nil, fmt.Errorf("pppvpn: invalid staged session %q", claim.Name())
		}
		dir := filepath.Join(staged, claim.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("pppvpn: read session claim: %w", err)
		}
		for _, entry := range entries {
			if entry.Name() != "record" || entry.IsDir() {
				return nil, fmt.Errorf("pppvpn: invalid session claim entry %q", entry.Name())
			}
			paths = append(paths, filepath.Join(dir, entry.Name()))
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
	staged := filepath.Join(rootDir(), "sessions-staged")
	if err := os.MkdirAll(staged, 0700); err != nil {
		return fmt.Errorf("pppvpn: create claim directory: %w", err)
	}
	if err := syncPPPDirectory(rootDir()); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(staged, "claim-")
	if err != nil {
		return fmt.Errorf("pppvpn: create session claim: %w", err)
	}
	if err := syncPPPDirectory(staged); err != nil {
		return err
	}
	claimed := filepath.Join(dir, "record")
	if err := os.Rename(sample.path, claimed); err != nil {
		return fmt.Errorf("pppvpn: claim session record %q: %w", sample.path, err)
	}
	if err := syncPPPDirectory(dir); err != nil {
		return err
	}
	if err := syncPPPDirectory(filepath.Dir(sample.path)); err != nil {
		return err
	}
	current, err := readPPPSession(claimed)
	if err != nil {
		return fmt.Errorf("pppvpn: inspect claimed session %q: %w", claimed, err)
	}
	if current != sample.session {
		return fmt.Errorf("pppvpn: session changed before acknowledgement; retained %q", claimed)
	}
	if err := os.Remove(claimed); err != nil {
		return fmt.Errorf("pppvpn: remove committed session claim: %w", err)
	}
	if err := syncPPPDirectory(dir); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil {
		return fmt.Errorf("pppvpn: remove empty session claim: %w", err)
	}
	if err := syncPPPDirectory(staged); err != nil {
		return err
	}
	return nil
}

func syncPPPDirectory(path string) error {
	f, err := os.Open(path)
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
	file, err := os.Open(path)
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
	if len(fields) != 7 || (fields[0] != "active" && fields[0] != "closed") || fields[1] == "" || fields[2] == "" || fields[5] == "" {
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
	return pppSession{iface: fields[1], email: fields[2], rx: rx, tx: tx, sessionID: fields[5], inboundID: inboundID, closed: fields[0] == "closed"}, nil
}

func readInterfaceBytes(iface string) (uint64, uint64, error) {
	if strings.ContainsAny(iface, "/\\\x00") || iface == "." || iface == ".." {
		return 0, 0, errors.New("pppvpn: invalid interface name")
	}
	base := filepath.Join("/sys/class/net", iface, "statistics")
	rx, err := readCounter(filepath.Join(base, "rx_bytes"))
	if err != nil {
		return 0, 0, err
	}
	tx, err := readCounter(filepath.Join(base, "tx_bytes"))
	return rx, tx, err
}

func readCounter(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
}
