package l2tp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// InboundTraffic is a byte delta for the L2TP inbound.
type InboundTraffic struct {
	InboundId int
	Tag       string
	Up        int64
	Down      int64
}

// ClientTrafficDelta attributes a byte delta to an existing panel client by
// email. The panel's normal traffic writer persists these deltas alongside
// Xray and OpenVPN traffic.
type ClientTrafficDelta struct {
	InboundId int
	Email     string
	Up        int64
	Down      int64
}

// CounterDecrease is an uncertain accounting interval, not a new session or
// an estimate of missing bytes. Callers must retain and reconcile evidence.
type CounterDecrease struct {
	InboundID                                      int
	Interface, Email                               string
	PreviousRx, PreviousTx, ObservedRx, ObservedTx int64
}

func (e *CounterDecrease) Error() string {
	return fmt.Sprintf("l2tp: counter decreased for inbound %d interface %q client %q (rx %d to %d, tx %d to %d); reconciliation required", e.InboundID, e.Interface, e.Email, e.PreviousRx, e.ObservedRx, e.PreviousTx, e.ObservedTx)
}

// SessionReadFailure records an unreadable or malformed L2TP observation.
// Previous baselines remain unchanged and no missing usage is estimated.
type SessionReadFailure struct {
	InboundID int
	Cause     string
	Previous  []SessionObservation
}

func (e *SessionReadFailure) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "l2tp: failed to read session counters for inbound %d: %s", e.InboundID, e.Cause)
	for _, observation := range e.Previous {
		fmt.Fprintf(&b, "; last sampled interface %q client %q rx %d tx %d", observation.Interface, observation.Email, observation.PreviousRx, observation.PreviousTx)
	}
	b.WriteString("; reconciliation required")
	return b.String()
}

// SessionDisappeared reports a missing sampled session or a session marker
// whose interface vanished before any sample. It does not estimate missing
// traffic or prove the session has terminated.
type SessionDisappeared struct {
	InboundID              int
	Interface, Email       string
	PreviousRx, PreviousTx int64
	NoBaseline             bool
	Cause                  string
	Additional             []SessionObservation
	Retained               []SessionObservation
	New                    []SessionObservation
}

// SessionObservation preserves another affected baseline from the same poll.
type SessionObservation struct {
	Interface, Email       string
	PreviousRx, PreviousTx int64
	ObservedRx, ObservedTx int64 // meaningful for Retained and New observations only
	NoBaseline             bool
}

func (e *SessionDisappeared) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "l2tp: session accounting uncertain for inbound %d (%s)", e.InboundID, e.Cause)
	writeMissing := func(iface, email string, rx, tx int64, noBaseline bool) {
		if noBaseline {
			fmt.Fprintf(&b, "; missing interface %q client %q with no prior sample", iface, email)
		} else {
			fmt.Fprintf(&b, "; missing interface %q client %q last sampled rx %d tx %d", iface, email, rx, tx)
		}
	}
	writeMissing(e.Interface, e.Email, e.PreviousRx, e.PreviousTx, e.NoBaseline)
	for _, observation := range e.Additional {
		writeMissing(observation.Interface, observation.Email, observation.PreviousRx, observation.PreviousTx, observation.NoBaseline)
	}
	for _, observation := range e.Retained {
		fmt.Fprintf(&b, "; currently present: interface %q client %q previous rx %d tx %d observed rx %d tx %d", observation.Interface, observation.Email, observation.PreviousRx, observation.PreviousTx, observation.ObservedRx, observation.ObservedTx)
	}
	for _, observation := range e.New {
		fmt.Fprintf(&b, "; no prior baseline: interface %q client %q observed rx %d tx %d", observation.Interface, observation.Email, observation.ObservedRx, observation.ObservedTx)
	}
	b.WriteString("; final usage requires reconciliation")
	return b.String()
}

type interfaceCounter struct {
	rx int64
	tx int64
}

// l2tpSysClassNetRoot is a variable so filesystem accounting tests can use a
// temporary interface tree. Production always points at Linux sysfs.
var l2tpSysClassNetRoot = "/sys/class/net"

func sessionDirForID(id int) string {
	return filepath.Join(dataDirForID(id), "sessions")
}

func validInterfaceName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

const maxInterfaceCounter = int64(^uint64(0) >> 1)

func readCounter(path string) (int64, bool) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, parseErr := strconv.ParseUint(strings.TrimSpace(string(contents)), 10, 64)
	if parseErr != nil {
		return 0, false
	}
	// Saturation would silently discard subsequent usage while the unsigned
	// kernel counter continues to grow. An unrepresentable sample is unsafe.
	if value > uint64(maxInterfaceCounter) {
		return 0, false
	}
	return int64(value), true
}

func readInterfaceCounter(iface string) (interfaceCounter, bool) {
	base := filepath.Join(l2tpSysClassNetRoot, iface, "statistics")
	rx, rxOK := readCounter(filepath.Join(base, "rx_bytes"))
	tx, txOK := readCounter(filepath.Join(base, "tx_bytes"))
	return interfaceCounter{rx: rx, tx: tx}, rxOK && txOK
}

func readSessions(id int) (map[string]interfaceCounter, []string, error) {
	if err := CheckDownHookIncidents(); err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(l2tpRoot())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]interfaceCounter{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open L2TP runtime root: %w", err)
	}
	defer root.Close()
	dir := filepath.Join(strconv.Itoa(id), "sessions")
	entries, err := readEvidenceDir(root, dir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]interfaceCounter{}, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read L2TP session directory: %w", err)
	}
	out := make(map[string]interfaceCounter, len(entries))
	var stale []string
	for _, entry := range entries {
		if entry.IsDir() || !validInterfaceName(entry.Name()) {
			return nil, nil, fmt.Errorf("invalid L2TP session marker %q", entry.Name())
		}
		email, err := readEvidenceEmail(root, filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, nil, err
		}
		// An absent interface is not proof of zero final usage. Retain the
		// marker identity even when this session was never sampled.
		if _, err := os.Stat(filepath.Join(l2tpSysClassNetRoot, entry.Name())); err != nil {
			if os.IsNotExist(err) {
				stale = append(stale, entry.Name()+"\x00"+email)
				continue
			}
			return nil, nil, fmt.Errorf("inspect L2TP interface %q: %w", entry.Name(), err)
		}
		counter, ok := readInterfaceCounter(entry.Name())
		if !ok {
			return nil, nil, fmt.Errorf("read L2TP counters for interface %q", entry.Name())
		}
		out[entry.Name()+"\x00"+email] = counter
	}
	if err := CheckDownHookIncidents(); err != nil {
		return nil, nil, err
	}
	return out, stale, nil
}

func saturatingCounterAdd(current, delta int64) int64 {
	if delta <= 0 {
		return current
	}
	if current >= maxInterfaceCounter-delta {
		return maxInterfaceCounter
	}
	return current + delta
}

func addDelta(dst map[string]ClientTrafficDelta, id int, email string, up, down int64) {
	if up == 0 && down == 0 {
		return
	}
	item := dst[email]
	item.InboundId = id
	item.Email = email
	item.Up = saturatingCounterAdd(item.Up, up)
	item.Down = saturatingCounterAdd(item.Down, down)
	dst[email] = item
}

func sortedClientDeltas(values map[string]ClientTrafficDelta) []ClientTrafficDelta {
	out := make([]ClientTrafficDelta, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}
