package openvpn

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
)

// InboundTraffic is the per-inbound byte delta for one polling round.
type InboundTraffic struct {
	InboundId int
	Tag       string
	Up        int64
	Down      int64
}

// ClientTrafficDelta is the per-client byte delta for one polling round.
// Email carries the client's CN (its email address), which the traffic
// pipeline uses to attribute usage exactly like xray-reported clients.
type ClientTrafficDelta struct {
	InboundId int
	Email     string
	Up        int64
	Down      int64
}

type managed struct {
	proc        *Process
	tag         string
	fingerprint string
	mgmtPort    int
	clients     []string

	// Last observed cumulative counters per client CN; deltas are computed
	// against these each round. Reset handling (daemon restart zeroes the
	// counters) is folded into the delta computation in CollectTraffic.
	lastRx   map[string]int64
	lastTx   map[string]int64
	lastID   map[string]string
	online   map[string]bool
	haveLast bool

	// Start-failure throttling: a broken daemon (missing binary, busy port)
	// must not spam the log every 10 s tick.
	lastErrAt time.Time
}

// Manager owns the set of running openvpn daemons keyed by inbound id.
type Manager struct {
	mu sync.Mutex
	// trafficMu serializes management polls. The poll itself runs without mu
	// because a management socket can take seconds to time out; the state update
	// is committed under mu after the result returns.
	trafficMu sync.Mutex
	// procs maps inbound id -> running (or last-started) daemon state.
	procs map[int]*managed

	// manualStop is controlled by the dashboard. A normal Stop() only stops
	// the current processes; the reconcile job would otherwise start them
	// again on its next ten-second tick. The flag is intentionally in-memory:
	// a full panel restart should return to the database's desired state.
	manualStop bool
	lastError  string
	// Current process-local poll failures, cleared only by a successful poll
	// for the same inbound. This is an operational warning, not a durable
	// accounting incident or proof that unobserved traffic was recovered.
	trafficPollFailures map[int]struct{}
	accountingFailed    bool
	// A stopped daemon, a disappeared session or a counter drop leaves final
	// usage unproven by status polls. Retain this warning for the process
	// lifetime; it is not durable incident evidence.
	unverifiedSessionTraffic bool
}

// RuntimeStatus is the small, secret-free snapshot exposed by the dashboard.
type RuntimeStatus struct {
	Running       bool
	InboundCount  int
	OnlineClients int
	ManualStop    bool
	Error         string
}

var (
	managerOnce sync.Once
	manager     *Manager
)

// GetManager returns the process-wide openvpn manager singleton.
func GetManager() *Manager {
	managerOnce.Do(func() {
		manager = &Manager{procs: map[int]*managed{}}
	})
	return manager
}

// Ensure brings one inbound's daemon to the desired state immediately.
func (m *Manager) Ensure(inst Instance) error {
	return m.ensure(inst)
}

// ensure brings one inbound's daemon to the desired state: starts it when
// missing or crashed, restarts it when the fingerprint (config + client
// certs) changed, and no-ops when it is already running with the same
// fingerprint. Certificates are (re)generated before every (re)start.
func (m *Manager) ensure(inst Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureLocked(inst)
}

func (m *Manager) ensureLocked(inst Instance) error {
	if err := inst.validateRuntimeConfig(); err != nil {
		return err
	}
	certMu.Lock()
	defer certMu.Unlock()
	fp := inst.fingerprint()
	dir := dataDirForID(inst.Id)
	if err := validateClientCertSetLocked(dir, inst.Clients); err != nil {
		return err
	}
	// Verify and provision all key material before stopping an existing daemon
	// or reporting a successful no-op. In particular, missing or corrupt CA
	// evidence must not be hidden by the already-running fast path.
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := ensureServerCertLocked(dir); err != nil {
		return err
	}
	for _, email := range inst.Clients {
		if err := ensureClientCertLocked(dir, email); err != nil {
			return err
		}
	}
	if cur, ok := m.procs[inst.Id]; ok && cur.fingerprint == fp && cur.proc.IsRunning() {
		if err := GetNetworkManager().Apply(inst); err != nil {
			return err
		}
		cur.tag = inst.Tag
		cur.clients = inst.Clients
		return nil
	}
	if cur, ok := m.procs[inst.Id]; ok {
		if err := cur.proc.Stop(); err != nil {
			return fmt.Errorf("stop existing OpenVPN daemon: %w", err)
		}
		m.noteStoppedTrafficLocked(cur)
		GetNetworkManager().Remove(inst.Id)
		delete(m.procs, inst.Id)
	}

	if err := pruneClientCertsLocked(dir, inst.Clients); err != nil {
		logger.Warningf("openvpn: inbound %d: prune client certs failed: %v", inst.Id, err)
	}

	mgmtPort, err := FreeLocalPort()
	if err != nil {
		return err
	}
	cfgPath := confPathForID(inst.Id)
	if err := writeConfig(inst, mgmtPort); err != nil {
		return err
	}
	if err := GetNetworkManager().Apply(inst); err != nil {
		return err
	}
	proc := newProcess(cfgPath, "inbound "+strconv.Itoa(inst.Id))
	if err := proc.Start(); err != nil {
		GetNetworkManager().Remove(inst.Id)
		m.procs[inst.Id] = &managed{
			proc:        proc,
			tag:         inst.Tag,
			fingerprint: fp,
			clients:     inst.Clients,
			lastErrAt:   time.Now(),
		}
		return err
	}
	m.procs[inst.Id] = &managed{
		proc:        proc,
		tag:         inst.Tag,
		fingerprint: fp,
		mgmtPort:    mgmtPort,
		clients:     inst.Clients,
		lastRx:      map[string]int64{},
		lastTx:      map[string]int64{},
		online:      map[string]bool{},
	}
	logger.Infof("openvpn: started daemon for inbound %d (port %d, %s, %d client(s))",
		inst.Id, inst.Port, inst.protoFor(), len(inst.Clients))
	return nil
}

// Reconcile drives the running set toward the desired instances: it stops
// daemons that are no longer wanted (and removes their data directory) and
// (re)starts the rest. Called periodically by the openvpn job. Retained PID
// files require manual reconciliation; they do not authorize orphan signalling.
// stopManagedLocked keeps a failed process and its network rules visible for
// subsequent retries. A stopped process is not proof of final accounting.
func (m *Manager) stopManagedLocked(id int) error {
	cur, ok := m.procs[id]
	if !ok {
		return nil
	}
	if err := cur.proc.Stop(); err != nil {
		if m.lastError == "" {
			m.lastError = fmt.Sprintf("inbound %d: stop failed: %v", id, err)
		}
		logger.Warningf("openvpn: inbound %d stop failed; retaining daemon and evidence: %v", id, err)
		return fmt.Errorf("inbound %d stop failed: %w", id, err)
	}
	m.noteStoppedTrafficLocked(cur)
	GetNetworkManager().Remove(id)
	delete(m.procs, id)
	return nil
}

// stopAllLocked preserves rules for any process whose stop was not confirmed.
func (m *Manager) stopAllLocked() error {
	keep := make(map[int]struct{})
	var stopErr error
	for id := range m.procs {
		if err := m.stopManagedLocked(id); err != nil {
			keep[id] = struct{}{}
			stopErr = errors.Join(stopErr, err)
		}
	}
	GetNetworkManager().RemoveExcept(keep)
	return stopErr
}

func (m *Manager) Reconcile(desired []Instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastError = ""
	if m.manualStop {
		m.stopAllLocked()
		return
	}
	want := make(map[int]struct{}, len(desired))
	for _, inst := range desired {
		want[inst.Id] = struct{}{}
	}
	for id := range m.procs {
		if _, ok := want[id]; !ok {
			if err := m.stopManagedLocked(id); err != nil {
				// Do not remove the rules of a daemon that may still be live.
				want[id] = struct{}{}
				continue
			}
			logger.Infof("openvpn: stopped daemon for inbound %d (no longer desired)", id)
		}
	}
	GetNetworkManager().RemoveExcept(want)
	// A later success must not hide an earlier stop or start failure.
	for _, inst := range desired {
		if err := m.ensureLocked(inst); err != nil {
			if m.lastError == "" {
				m.lastError = fmt.Sprintf("inbound %d: %v", inst.Id, err)
			}
			// Throttled: a persistent failure (no binary, busy port) logs
			// at most once a minute instead of every 10 s tick.
			if cur, ok := m.procs[inst.Id]; ok && time.Since(cur.lastErrAt) < time.Minute {
				continue
			}
			if cur, ok := m.procs[inst.Id]; ok {
				cur.lastErrAt = time.Now()
			}
			logger.Warningf("openvpn: reconcile failed for inbound %d: %v", inst.Id, err)
		}
	}
}

// StopManually stops every OpenVPN daemon and holds reconciliation until the
// dashboard explicitly resumes it. Preserves the per-inbound PKI directory so
// existing client .ovpn profiles remain valid when resumed.
func (m *Manager) StopManually() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manualStop = true
	m.lastError = ""
	return m.stopAllLocked()
}

// Resume clears a dashboard stop request. The next reconcile tick starts the
// enabled inbounds again from the database's desired state.
func (m *Manager) Resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manualStop = false
	m.lastError = ""
}

// Restart stops the current daemons without setting manualStop, allowing the
// following reconcile pass to create fresh processes from current settings
// while preserving the per-inbound PKI CA and client certificates.
func (m *Manager) Restart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manualStop = false
	m.lastError = ""
	return m.stopAllLocked()
}

// StopInbound stops the daemon and removes firewall rules for an inbound id
// without deleting its PKI certificates on disk (used when an inbound is
// disabled rather than deleted).
func (m *Manager) StopInbound(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.stopManagedLocked(id); err != nil {
		return err
	}
	if _, err := os.Lstat(pidPathForID(id)); err == nil {
		err := fmt.Errorf("inbound %d: retained PID evidence requires manual reconciliation", id)
		m.lastError = err.Error()
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect inbound %d PID evidence: %w", id, err)
	}
	GetNetworkManager().Remove(id)
	return nil
}

// SetAccountingResult updates a process-local operational warning. The
// underlying error can contain sensitive database or filesystem details and is
// logged by the caller, never included in dashboard status. A nil result only
// means the latest accounting operation succeeded; it does not prove complete
// accounting for any earlier interval.
func (m *Manager) SetAccountingResult(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accountingFailed = err != nil
}

// Status returns a secret-free snapshot for the dashboard.
func (m *Manager) Status() RuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := RuntimeStatus{ManualStop: m.manualStop, Error: m.lastError}
	if len(m.trafficPollFailures) > 0 {
		warning := fmt.Sprintf("OpenVPN traffic poll incomplete for %d inbound(s); missing usage may require review", len(m.trafficPollFailures))
		if status.Error != "" {
			status.Error += "\n"
		}
		status.Error += warning
	}
	if m.accountingFailed {
		if status.Error != "" {
			status.Error += "\n"
		}
		status.Error += "OpenVPN traffic accounting unavailable; pending data or recovery may require review"
	}
	if m.unverifiedSessionTraffic {
		if status.Error != "" {
			status.Error += "\n"
		}
		status.Error += "OpenVPN daemon stopped or session changed, disappeared or counters decreased; final usage is unverified"
	}
	seen := make(map[string]struct{})
	for _, cur := range m.procs {
		if cur.proc != nil && cur.proc.IsRunning() {
			status.Running = true
			status.InboundCount++
			for email := range cur.online {
				seen[email] = struct{}{}
			}
		}
	}
	status.OnlineClients = len(seen)
	return status
}

// Remove stops and forgets the daemon for an inbound id, deleting its data
// directory. Used when an inbound is deleted.
func (m *Manager) Remove(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.stopManagedLocked(id); err != nil {
		return err
	}
	GetNetworkManager().Remove(id)
	if err := removeDataDir(id); err != nil {
		if m.lastError == "" {
			m.lastError = fmt.Sprintf("inbound %d: retained data for manual review: %v", id, err)
		}
		logger.Warningf("openvpn: inbound %d retained data for manual review: %v", id, err)
		return fmt.Errorf("inbound %d retained data for manual review: %w", id, err)
	}
	return nil
}

// StopAll stops every managed daemon. Called on panel shutdown.
func (m *Manager) StopAll() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manualStop = false
	m.lastError = ""
	return m.stopAllLocked()
}

func removeDataDir(id int) error {
	certMu.Lock()
	defer certMu.Unlock()
	if _, err := os.Lstat(pidPathForID(id)); err == nil {
		return fmt.Errorf("retained PID evidence requires manual reconciliation")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect PID evidence: %w", err)
	}
	return os.RemoveAll(dataDirForID(id))
}

// CollectTraffic polls each running daemon's management interface and
// returns per-inbound and per-client byte deltas since the previous poll.
// Daemons whose management interface is unreachable contribute nothing (and
// drop their clients from the online set).
func (m *Manager) CollectTraffic() (inbounds []InboundTraffic, clients []ClientTrafficDelta) {
	inbounds, clients, _ = m.CollectTrafficWithErrors()
	return
}

// noteStoppedTrafficLocked marks a successfully started daemon's final usage
// unverified. A last status poll does not fence traffic before the stop, and a
// session can start and end entirely between polls. Failed starts have no
// management port and are not evidence that such a daemon actually ran.
// m.mu must be held.
func (m *Manager) noteStoppedTrafficLocked(man *managed) {
	if man != nil && man.mgmtPort > 0 {
		m.unverifiedSessionTraffic = true
	}
}

// markTrafficPollFailureLocked records no raw socket data or client identity.
// m.mu must be held. The flag survives reconcile/stop within this process.
func (m *Manager) markTrafficPollFailureLocked(id int) {
	if m.trafficPollFailures == nil {
		m.trafficPollFailures = make(map[int]struct{})
	}
	m.trafficPollFailures[id] = struct{}{}
}

// CollectTrafficWithErrors returns usable deltas even when another daemon's
// poll failed. Callers must commit those deltas before acting on the error;
// discarding them would discard already-advanced in-memory baselines.
func (m *Manager) CollectTrafficWithErrors() (inbounds []InboundTraffic, clients []ClientTrafficDelta, pollErr error) {
	// Do not let two scheduler/dashboard callers consume the same cumulative
	// counters concurrently. More importantly, this keeps the snapshot/query/
	// commit sequence ordered while still allowing status and reconcile to run
	// during a slow management-socket timeout.
	m.trafficMu.Lock()
	defer m.trafficMu.Unlock()

	m.mu.Lock()
	type probe struct {
		id       int
		tag      string
		mgmtPort int
		man      *managed
	}
	probes := make([]probe, 0, len(m.procs))
	for id, man := range m.procs {
		// A nil proc only happens in tests; in production every entry has
		// one. A dead daemon is handled by the stale-online path below.
		if man.mgmtPort > 0 && (man.proc == nil || man.proc.IsRunning()) {
			probes = append(probes, probe{id: id, tag: man.tag, mgmtPort: man.mgmtPort, man: man})
			continue
		}
		// Not running: a started daemon may have exited with unpolled traffic,
		// even if the last snapshot contained no clients.
		m.noteStoppedTrafficLocked(man)
		// Its online set is stale. Status and OnlineEmails use the same lock.
		clearOnline(man)
	}
	m.mu.Unlock()

	for _, p := range probes {
		// The socket query deliberately runs without m.mu. Once it returns,
		// reacquire m.mu and verify that reconcile has not replaced this managed
		// entry while the query was in flight. A result from an old daemon must
		// never be applied to a newly started daemon for the same inbound.
		list, err := clientList(p.mgmtPort)

		m.mu.Lock()
		man, current := m.procs[p.id]
		if !current || man != p.man {
			// A discarded sample is not a complete poll. Propagate the gap so
			// callers commit any other deltas but defer reconciliation rather
			// than treating an old daemon's counters as safely accounted for.
			pollErr = errors.Join(pollErr, fmt.Errorf("inbound %d: daemon changed during management traffic poll", p.id))
			m.noteStoppedTrafficLocked(p.man)
			m.markTrafficPollFailureLocked(p.id)
			m.mu.Unlock()
			continue
		}
		if err != nil {
			pollErr = errors.Join(pollErr, fmt.Errorf("inbound %d: %w", p.id, err))
			m.markTrafficPollFailureLocked(p.id)
			logger.Warningf("openvpn: inbound %d management traffic poll failed; counters retained: %v", p.id, err)
			clearOnline(man)
			m.mu.Unlock()
			continue
		}
		// Stage the whole daemon snapshot before touching baselines or emitting
		// client deltas. Saturating the aggregate silently loses traffic and
		// committing only a prefix would make a retry double-count that prefix.
		unverified := false
		if man.haveLast {
			for cn, previousRx := range man.lastRx {
				current, ok := list[cn]
				previousTx, txOK := man.lastTx[cn]
				previousID := man.lastID[cn]
				if !ok || !txOK || current.Rx < previousRx || current.Tx < previousTx || current.ID != previousID {
					unverified = true
				}
			}
			for cn := range man.lastTx {
				if _, ok := man.lastRx[cn]; !ok {
					unverified = true
				}
			}
		}
		up, down := int64(0), int64(0)
		newOnline := make(map[string]bool, len(list))
		newRx, newTx := make(map[string]int64, len(list)), make(map[string]int64, len(list))
		newID := make(map[string]string, len(list))
		var stagedClients []ClientTrafficDelta
		overflow := false
		for cn, c := range list {
			newOnline[cn] = true
			var baseRx, baseTx int64
			if man.haveLast {
				baseRx, baseTx = man.lastRx[cn], man.lastTx[cn]
				// A different known CID is a different client instance. Its
				// counters must not be subtracted from the old session's.
				if previousID := man.lastID[cn]; previousID != "" && c.ID != "" && previousID != c.ID {
					baseRx, baseTx = 0, 0
				}
			}
			// A lower reading is treated as a reset, as before.
			dRx, dTx := c.Rx, c.Tx
			if c.Rx >= baseRx {
				dRx = c.Rx - baseRx
			}
			if c.Tx >= baseTx {
				dTx = c.Tx - baseTx
			}
			if dRx > maxManagementCounter-up || dTx > maxManagementCounter-down {
				overflow = true
				break
			}
			up += dRx
			down += dTx
			if dRx > 0 || dTx > 0 {
				stagedClients = append(stagedClients, ClientTrafficDelta{
					InboundId: p.id, Email: cn, Up: dRx, Down: dTx,
				})
			}
			newRx[cn], newTx[cn], newID[cn] = c.Rx, c.Tx, c.ID
		}
		if overflow {
			pollErr = errors.Join(pollErr, fmt.Errorf("inbound %d: management traffic delta sum exceeds supported range", p.id))
			m.markTrafficPollFailureLocked(p.id)
			clearOnline(man)
			m.mu.Unlock()
			continue
		}
		clients = append(clients, stagedClients...)
		delete(m.trafficPollFailures, p.id)
		m.unverifiedSessionTraffic = m.unverifiedSessionTraffic || unverified
		man.lastRx, man.lastTx, man.lastID = newRx, newTx, newID
		man.online = newOnline
		man.haveLast = true
		if up > 0 || down > 0 {
			inbounds = append(inbounds, InboundTraffic{InboundId: p.id, Tag: p.tag, Up: up, Down: down})
		}
		m.mu.Unlock()
	}
	return inbounds, clients, pollErr
}

func clearOnline(man *managed) {
	if man == nil || man.online == nil {
		return
	}
	for cn := range man.online {
		delete(man.online, cn)
	}
}

// OnlineEmails returns the union of connected client CNs across all running
// daemons — the openvpn half of the panel's online-client set. A client
// attached to several openvpn inbounds is listed once.
func (m *Manager) OnlineEmails() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[string]bool, 4)
	out := make([]string, 0, 4)
	for _, man := range m.procs {
		for cn := range man.online {
			if !seen[cn] {
				seen[cn] = true
				out = append(out, cn)
			}
		}
	}
	return out
}
