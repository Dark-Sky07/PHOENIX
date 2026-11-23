package l2tp

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
)

type managed struct {
	proc        *Process
	tag         string
	fingerprint string
	lastErrAt   time.Time
	last        map[string]interfaceCounter
	online      map[string]bool
}

// Manager owns the one global L2TP/IPsec process group. The service layer also
// enforces the singleton in the database; keeping the guard here prevents a
// malformed reconcile input from starting two listeners during a race.
type Manager struct {
	mu    sync.Mutex
	proc  *managed
	id    int
	ready bool

	// manualStop prevents the periodic reconcile job from immediately starting
	// the daemon after an explicit dashboard stop. It is intentionally reset by
	// a full panel process restart.
	manualStop bool
	lastError  string
}

// RuntimeStatus is the secret-free L2TP/IPsec snapshot exposed by the panel.
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

func GetManager() *Manager {
	managerOnce.Do(func() { manager = &Manager{} })
	return manager
}

// orphanDataError conservatively protects runtime evidence left by a previous
// panel process. A PID file or process name cannot establish ownership (PIDs
// can be reused, and strongSwan uses a host-global PID directory). Do not
// signal or delete an untracked group automatically. The tracked directory is
// exempt only while this Manager still owns its process handles.
func orphanDataError(trackedID int) error {
	entries, err := os.ReadDir(l2tpRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect L2TP runtime directories: %w", err)
	}
	for _, entry := range entries {
		id, err := strconv.Atoi(entry.Name())
		if err != nil || id <= 0 {
			continue
		}
		if id == trackedID && entry.Name() == strconv.Itoa(trackedID) {
			continue
		}
		return fmt.Errorf("untracked L2TP runtime directory %q requires manual review; refusing automatic cleanup", entry.Name())
	}
	return nil
}

func (m *Manager) orphanDataErrorLocked() error {
	trackedID := 0
	if m.proc != nil {
		trackedID = m.id
	}
	return orphanDataError(trackedID)
}

// CheckUntrackedRuntime reports locally visible orphan or hook evidence
// without signalling or modifying it. A clean result is not proof of quiescence.
func (m *Manager) CheckUntrackedRuntime() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.orphanDataErrorLocked(); err != nil {
		return err
	}
	return CheckHookIncidents()
}

// Ensure brings the daemon and firewall toward one desired instance.
func (m *Manager) Ensure(inst Instance) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.ensureLocked(inst)
	if err != nil {
		m.lastError = err.Error()
	} else {
		m.lastError = ""
	}
	return err
}

func (m *Manager) ensureLocked(inst Instance) error {
	if err := ValidateSettings(inst); err != nil {
		return err
	}
	if err := m.orphanDataErrorLocked(); err != nil {
		return err
	}
	if m.proc != nil && m.id != inst.Id {
		return fmt.Errorf("remove tracked L2TP inbound %d before starting inbound %d", m.id, inst.Id)
	}
	fp := inst.fingerprint()
	if m.proc != nil && m.id == inst.Id && m.proc.fingerprint == fp && m.proc.proc.IsRunning() {
		if err := reconcileSystemChapSecrets(inst); err != nil {
			return err
		}
		m.proc.tag = inst.Tag
		if m.proc.last == nil {
			m.proc.last = make(map[string]interfaceCounter)
		}
		if m.proc.online == nil {
			m.proc.online = make(map[string]bool)
		}
		if err := GetNetworkManager().Apply(inst); err != nil {
			return err
		}
		return nil
	}
	if m.proc != nil {
		if err := m.proc.proc.Stop(); err != nil {
			return fmt.Errorf("stop l2tp daemon group for inbound %d: %w", m.id, err)
		}
		GetNetworkManager().Remove(m.id)
		removeStrongSwanRuntime(m.id)
		_ = clearSystemChapSecrets()
		m.proc = nil
		m.id = 0
	}
	if m.proc == nil {
		if err := clearSystemChapSecrets(); err != nil {
			return err
		}
	}
	GetNetworkManager().Remove(inst.Id)
	if err := writeConfig(inst); err != nil {
		return err
	}
	if err := GetNetworkManager().Apply(inst); err != nil {
		_ = clearSystemChapSecrets()
		return err
	}
	proc := newProcess(inst.Id)
	if err := proc.Start(); err != nil {
		GetNetworkManager().Remove(inst.Id)
		_ = clearSystemChapSecrets()
		m.proc = &managed{proc: proc, tag: inst.Tag, fingerprint: fp, lastErrAt: time.Now(), last: map[string]interfaceCounter{}, online: map[string]bool{}}
		m.id = inst.Id
		return err
	}
	m.proc = &managed{proc: proc, tag: inst.Tag, fingerprint: fp, last: map[string]interfaceCounter{}, online: map[string]bool{}}
	m.id = inst.Id
	m.ready = true
	logger.Infof("l2tp: started strongSwan/xl2tpd for inbound %d on UDP 500, 4500 and 1701", inst.Id)
	return nil
}

// Reconcile keeps the daemon group aligned with the enabled local inbound.
// More than one desired instance is a programming/data error; only the first
// is considered so the fixed ports can never be double-bound.
func (m *Manager) Reconcile(desired []Instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.orphanDataErrorLocked(); err != nil {
		m.lastError = err.Error()
		logger.Warningf("l2tp: reconciliation deferred: %v", err)
		return
	}
	if m.manualStop {
		if m.proc != nil {
			if err := m.proc.proc.Stop(); err != nil {
				m.lastError = err.Error()
				logger.Warningf("l2tp: cannot reconcile stopped daemon group: %v", err)
				return
			}
			GetNetworkManager().Remove(m.id)
			removeStrongSwanRuntime(m.id)
			_ = os.RemoveAll(dataDirForID(m.id))
			m.proc = nil
			m.id = 0
			m.ready = false
		}
		GetNetworkManager().RemoveAll()
		return
	}
	if len(desired) == 0 {
		m.lastError = ""
		if m.proc != nil {
			if err := m.proc.proc.Stop(); err != nil {
				m.lastError = err.Error()
				logger.Warningf("l2tp: cannot reconcile stopped daemon group: %v", err)
				return
			}
			GetNetworkManager().Remove(m.id)
			removeStrongSwanRuntime(m.id)
			_ = os.RemoveAll(dataDirForID(m.id))
			m.proc = nil
			m.id = 0
			m.ready = false
		}
		if err := clearSystemChapSecrets(); err != nil {
			logger.Warningf("l2tp: failed to clear system PPP chap-secrets: %v", err)
		}
		return
	}
	if len(desired) > 1 {
		logger.Warning("l2tp: more than one desired inbound was supplied; only the first will be reconciled")
	}
	inst := desired[0]
	err := m.ensureLocked(inst)
	if err != nil {
		m.lastError = err.Error()
		if m.proc != nil && time.Since(m.proc.lastErrAt) < time.Minute {
			return
		}
		if m.proc != nil {
			m.proc.lastErrAt = time.Now()
		}
		logger.Warningf("l2tp: reconcile failed for inbound %d: %v", inst.Id, err)
	} else {
		m.lastError = ""
	}
}

// StopManually stops the L2TP/IPsec daemon group and holds reconciliation
// until Resume is called by the dashboard.
func (m *Manager) StopManually() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manualStop = true
	m.lastError = ""
	if err := m.orphanDataErrorLocked(); err != nil {
		m.lastError = err.Error()
		return err
	}
	if m.proc != nil {
		if err := m.proc.proc.Stop(); err != nil {
			m.lastError = err.Error()
			return fmt.Errorf("stop l2tp daemon group for inbound %d: %w", m.id, err)
		}
		GetNetworkManager().Remove(m.id)
		removeStrongSwanRuntime(m.id)
		_ = os.RemoveAll(dataDirForID(m.id))
		m.proc = nil
		m.id = 0
		m.ready = false
	}
	GetNetworkManager().RemoveAll()
	_ = clearSystemChapSecrets()
	return nil
}

// Resume clears an explicit dashboard stop request. The next reconcile tick
// starts the enabled L2TP inbound again.
func (m *Manager) Resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manualStop = false
	m.lastError = ""
}

// Restart stops the current daemon group while leaving reconciliation enabled.
func (m *Manager) Restart() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manualStop = false
	m.lastError = ""
	if err := m.orphanDataErrorLocked(); err != nil {
		m.lastError = err.Error()
		return err
	}
	if m.proc != nil {
		if err := m.proc.proc.Stop(); err != nil {
			m.lastError = err.Error()
			return fmt.Errorf("stop l2tp daemon group for inbound %d: %w", m.id, err)
		}
		GetNetworkManager().Remove(m.id)
		removeStrongSwanRuntime(m.id)
		_ = os.RemoveAll(dataDirForID(m.id))
		m.proc = nil
		m.id = 0
		m.ready = false
	}
	GetNetworkManager().RemoveAll()
	_ = clearSystemChapSecrets()
	return nil
}

// Status returns the secret-free state used by the dashboard.
func (m *Manager) Status() RuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := RuntimeStatus{ManualStop: m.manualStop, Error: m.lastError}
	if m.proc != nil && m.proc.proc != nil {
		status.Running = m.proc.proc.IsRunning()
		if status.Running {
			status.InboundCount = 1
			status.OnlineClients = len(m.proc.online)
		}
	}
	return status
}

func (m *Manager) RemoveChecked(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.orphanDataErrorLocked(); err != nil {
		m.lastError = err.Error()
		return err
	}
	if m.proc != nil && (id == 0 || m.id == id) {
		if err := m.proc.proc.Stop(); err != nil {
			m.lastError = err.Error()
			return fmt.Errorf("stop l2tp daemon group for inbound %d: %w", m.id, err)
		}
		GetNetworkManager().Remove(m.id)
		removeStrongSwanRuntime(m.id)
		_ = os.RemoveAll(dataDirForID(m.id))
		m.proc = nil
		m.id = 0
		m.ready = false
	}
	if id == 0 {
		if err := clearSystemChapSecrets(); err != nil {
			logger.Warningf("l2tp: failed to clear system PPP chap-secrets: %v", err)
		}
		return nil
	}
	if m.proc == nil || m.id == id {
		GetNetworkManager().Remove(id)
		removeStrongSwanRuntime(id)
		_ = os.RemoveAll(dataDirForID(id))
	}
	if m.proc == nil {
		if err := clearSystemChapSecrets(); err != nil {
			logger.Warningf("l2tp: failed to clear system PPP chap-secrets: %v", err)
		}
	}
	return nil
}

func (m *Manager) Remove(id int) {
	if err := m.RemoveChecked(id); err != nil {
		logger.Warningf("l2tp: remove inbound %d failed: %v", id, err)
	}
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	m.manualStop = false
	m.lastError = ""
	m.mu.Unlock()
	if err := m.RemoveChecked(0); err != nil {
		logger.Warningf("l2tp: stop all failed: %v", err)
		return
	}
	GetNetworkManager().RemoveAll()
}

// CollectTraffic reads pppd session markers and Linux interface counters. pppd
// invokes the managed ip-up/ip-down hooks with PEERNAME and the interface name
// in $1/$IFNAME (some distro wrappers also export PPP_IFACE), which gives us a
// stable email-to-interface mapping without inventing a profile or requiring a
// RADIUS accounting server.
func (m *Manager) CollectTraffic() (inbounds []InboundTraffic, clients []ClientTrafficDelta) {
	inbounds, clients, _ = m.CollectTrafficChecked()
	return inbounds, clients
}

// CollectTrafficChecked reports a failed read without advancing baselines, so
// the caller can defer a destructive reconciliation until the next sample.
func (m *Manager) CollectTrafficChecked() (inbounds []InboundTraffic, clients []ClientTrafficDelta, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.proc == nil {
		if err := CheckHookIncidents(); err != nil {
			return nil, nil, l2tpReadFailure(m.id, nil, err)
		}
		return nil, nil, nil
	}
	if m.proc.proc == nil || !m.proc.proc.IsRunning() {
		// Parent exit is not proof that pppd children stopped or that their
		// final traffic was recorded. Preserve all known baselines as evidence.
		if missing := missingL2TPSessions(m.id, m.proc.last, nil, nil, "tracked runtime unavailable"); missing != nil {
			return nil, nil, missing
		}
		if err := CheckHookIncidents(); err != nil {
			return nil, nil, l2tpReadFailure(m.id, m.proc.last, err)
		}
		m.proc.online = map[string]bool{}
		return nil, nil, nil
	}
	if m.proc.last == nil {
		m.proc.last = make(map[string]interfaceCounter)
	}
	if m.proc.online == nil {
		m.proc.online = make(map[string]bool)
	}
	current, stale, readErr := readSessions(m.id)
	if readErr != nil {
		return nil, nil, l2tpReadFailure(m.id, m.proc.last, readErr)
	}
	// A missing previously sampled session is not a zero-traffic disconnect:
	// the down hook discards its marker without recording final counters.
	if missing := missingL2TPSessions(m.id, m.proc.last, current, stale, "session absent from sample or interface missing"); missing != nil {
		return nil, nil, missing
	}
	// Validate the entire snapshot before changing any baseline. A lower
	// counter can reflect interface reuse or reset, neither of which proves
	// the missing interval was zero or that the observed counter is new usage.
	keys := make([]string, 0, len(current))
	for key := range current {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		previous, exists := m.proc.last[key]
		counter := current[key]
		if exists && (counter.rx < previous.rx || counter.tx < previous.tx) {
			iface, email, _ := strings.Cut(key, "\x00")
			return nil, nil, &CounterDecrease{InboundID: m.id, Interface: iface, Email: email,
				PreviousRx: previous.rx, PreviousTx: previous.tx, ObservedRx: counter.rx, ObservedTx: counter.tx}
		}
	}
	newOnline := make(map[string]bool, len(current))
	deltaByEmail := make(map[string]ClientTrafficDelta)
	var totalUp, totalDown int64
	for key, counter := range current {
		separator := strings.IndexByte(key, 0)
		if separator < 0 {
			continue
		}
		email := key[separator+1:]
		newOnline[email] = true
		previous := m.proc.last[key]
		up, down := counter.rx, counter.tx
		if counter.rx >= previous.rx {
			up = counter.rx - previous.rx
		}
		if counter.tx >= previous.tx {
			down = counter.tx - previous.tx
		}
		addDelta(deltaByEmail, m.id, email, up, down)
		totalUp = saturatingCounterAdd(totalUp, up)
		totalDown = saturatingCounterAdd(totalDown, down)
		m.proc.last[key] = counter
	}
	for key := range m.proc.last {
		if _, ok := current[key]; !ok {
			delete(m.proc.last, key)
		}
	}
	m.proc.online = newOnline
	if totalUp > 0 || totalDown > 0 {
		inbounds = append(inbounds, InboundTraffic{InboundId: m.id, Tag: m.proc.tag, Up: totalUp, Down: totalDown})
	}
	return inbounds, sortedClientDeltas(deltaByEmail), nil
}

// l2tpReadFailure distinguishes an in-progress hook from a snapshot that
// needs permanent review; neither case advances or discards any baseline.
func l2tpReadFailure(id int, last map[string]interfaceCounter, readErr error) error {
	var pending *HookPendingError
	if errors.As(readErr, &pending) {
		return pending
	}
	failure := &SessionReadFailure{InboundID: id, Cause: readErr.Error()}
	keys := make([]string, 0, len(last))
	for key := range last {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		iface, email, _ := strings.Cut(key, "\x00")
		counter := last[key]
		failure.Previous = append(failure.Previous, SessionObservation{Interface: iface, Email: email, PreviousRx: counter.rx, PreviousTx: counter.tx})
	}
	return failure
}

// missingL2TPSessions retains every absent baseline in deterministic order.
// A nil current map marks all known sessions uncertain when runtime is down.
func missingL2TPSessions(id int, last, current map[string]interfaceCounter, stale []string, cause string) *SessionDisappeared {
	keys := make([]string, 0, len(last)+len(stale))
	seen := make(map[string]bool, len(last)+len(stale))
	retainedKeys := make([]string, 0, len(last))
	for key := range last {
		if _, present := current[key]; !present {
			keys = append(keys, key)
			seen[key] = true
		} else {
			retainedKeys = append(retainedKeys, key)
		}
	}
	for _, key := range stale {
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	sort.Strings(keys)
	sort.Strings(retainedKeys)
	if len(keys) == 0 {
		return nil
	}
	observation := func(key string) SessionObservation {
		iface, email, _ := strings.Cut(key, "\x00")
		counter, known := last[key]
		return SessionObservation{Interface: iface, Email: email, PreviousRx: counter.rx, PreviousTx: counter.tx, NoBaseline: !known}
	}
	first := observation(keys[0])
	missing := &SessionDisappeared{InboundID: id, Interface: first.Interface, Email: first.Email, PreviousRx: first.PreviousRx, PreviousTx: first.PreviousTx, NoBaseline: first.NoBaseline, Cause: cause}
	for _, key := range keys[1:] {
		missing.Additional = append(missing.Additional, observation(key))
	}
	for _, key := range retainedKeys {
		retained := observation(key)
		retained.ObservedRx, retained.ObservedTx = current[key].rx, current[key].tx
		missing.Retained = append(missing.Retained, retained)
	}
	var newKeys []string
	for key := range current {
		if _, exists := last[key]; !exists {
			newKeys = append(newKeys, key)
		}
	}
	sort.Strings(newKeys)
	for _, key := range newKeys {
		iface, email, _ := strings.Cut(key, "\x00")
		missing.New = append(missing.New, SessionObservation{Interface: iface, Email: email, ObservedRx: current[key].rx, ObservedTx: current[key].tx})
	}
	return missing
}

// OnlineEmails returns the currently connected L2TP usernames, deduplicated
// when a panel client has more than one PPP session.
func (m *Manager) OnlineEmails() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[string]bool)
	for email := range m.procOnline() {
		seen[email] = true
	}
	out := make([]string, 0, len(seen))
	for email := range seen {
		out = append(out, email)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) procOnline() map[string]bool {
	if m.proc == nil {
		return nil
	}
	return m.proc.online
}

func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proc != nil && m.proc.proc.IsRunning()
}
