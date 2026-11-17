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
	closed           []string
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
	if trafficCollector.pending != nil {
		return cloneTrafficBatch(trafficCollector.pending), nil
	}

	files, err := filepath.Glob(filepath.Join(rootDir(), "sessions", "*", "*"))
	if err != nil {
		return nil, err
	}
	batch := &TrafficBatch{}
	for _, path := range files {
		if strings.HasPrefix(filepath.Base(path), ".") {
			continue
		}
		session, readErr := readPPPSession(path)
		if readErr != nil {
			continue // Never delete an unreadable record that may hold final counters.
		}
		if !session.closed {
			session.rx, session.tx, err = readInterfaceBytes(session.iface)
			if err != nil {
				continue // interface is temporarily absent; retry on the next poll
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
			batch.closed = append(batch.closed, path)
			batch.ClosedSessionIDs = append(batch.ClosedSessionIDs, session.sessionID)
		}
	}
	trafficCollector.token++
	batch.Token = trafficCollector.token
	trafficCollector.pending = batch
	return cloneTrafficBatch(batch), nil
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
	for _, path := range trafficCollector.pending.closed {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && ackErr == nil {
			ackErr = fmt.Errorf("pppvpn: remove completed session record: %w", err)
		}
	}
	// DB persistence succeeded before AckTraffic. If cleanup fails, the same
	// absolute counters are safe to submit again because their DB checkpoints
	// were committed atomically with the last usage update.
	trafficCollector.pending = nil
	return ackErr
}

func cloneTrafficBatch(batch *TrafficBatch) *TrafficBatch {
	if batch == nil {
		return nil
	}
	copyBatch := &TrafficBatch{Token: batch.Token, Clients: make([]*xray.ClientTraffic, 0, len(batch.Clients)), ClosedSessionIDs: append([]string(nil), batch.ClosedSessionIDs...), closed: append([]string(nil), batch.closed...)}
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
	fields := strings.Split(scanner.Text(), "\t")
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
