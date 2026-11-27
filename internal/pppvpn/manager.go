package pppvpn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
)

type managed struct {
	instance    Instance
	fingerprint string
	process     *Process
	lastErr     time.Time
}
type Manager struct {
	mu         sync.Mutex
	inbounds   map[int]*managed
	manualStop bool
	lastError  string
}

// RuntimeStatus is the dashboard-facing summary of the local SSTP/PPTP
// daemon group. It deliberately contains no client identities or file paths.
type RuntimeStatus struct {
	Running    bool
	Error      string
	ManualStop bool
}

var managerOnce sync.Once
var singleton *Manager

func GetManager() *Manager {
	managerOnce.Do(func() { singleton = &Manager{inbounds: map[int]*managed{}} })
	return singleton
}

// UntrackedRuntimeError means local runtime files are not owned by this
// manager. File presence is evidence to investigate, not proof a daemon lives.
type UntrackedRuntimeError struct{ Detail string }

func (e *UntrackedRuntimeError) Error() string {
	return "pppvpn: untracked runtime requires manual review: " + e.Detail
}

func (m *Manager) checkUntrackedRuntimeLocked() error {
	for _, protocol := range []Protocol{SSTP, PPTP} {
		dir := filepath.Join(rootDir(), string(protocol))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return &UntrackedRuntimeError{Detail: fmt.Sprintf("inspect %s: %v", dir, err)}
		}
		for _, entry := range entries {
			id, err := strconv.Atoi(entry.Name())
			old := m.inbounds[id]
			if err == nil && id > 0 && entry.Name() == strconv.Itoa(id) && entry.IsDir() && old != nil && old.process != nil && old.instance.Protocol == protocol {
				continue
			}
			return &UntrackedRuntimeError{Detail: filepath.Join(dir, entry.Name())}
		}
	}
	return nil
}

// CheckUntrackedRuntime does not signal or clean up an unknown process. A
// clean directory scan is not proof of process ownership or session drain.
func (m *Manager) CheckUntrackedRuntime() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkUntrackedRuntimeLocked()
}

func (m *Manager) Ensure(i Instance) error {
	if err := Validate(i); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inbounds == nil {
		m.inbounds = map[int]*managed{}
	}
	if err := m.checkUntrackedRuntimeLocked(); err != nil {
		return err
	}
	fp := i.fingerprint()
	if old := m.inbounds[i.ID]; old != nil && old.fingerprint == fp && old.process != nil && old.process.IsRunning() {
		if old.instance.XrayRelayPort != i.XrayRelayPort {
			networkManager().removeInstance(old.instance)
		}
		if err := networkManager().apply(i); err != nil {
			return err
		}
		old.instance = i
		return nil
	}
	if old := m.inbounds[i.ID]; old != nil {
		if old.process != nil {
			if err := old.process.Stop(); err != nil {
				// Keep the old process and configuration tracked. Starting a
				// replacement while its exit is uncertain can overlap sessions.
				return fmt.Errorf("stop old %s daemon for inbound %d: %w", i.Protocol, i.ID, err)
			}
		}
		networkManager().removeInstance(old.instance)
		removeChapSecrets(old.instance)
		delete(m.inbounds, i.ID)
	}
	cfg, err := writeConfig(i)
	if err != nil {
		removeChapSecrets(i)
		_ = removeInstanceDir(i)
		return err
	}
	if err = networkManager().apply(i); err != nil {
		removeChapSecrets(i)
		_ = removeInstanceDir(i)
		return err
	}
	proc := &Process{}
	if err = proc.Start(i, cfg); err != nil {
		// Even a failed startup may have launched child processes. Preserve
		// ownership and evidence rather than silently deleting runtime files.
		m.inbounds[i.ID] = &managed{instance: i, fingerprint: fp, process: proc}
		return err
	}
	m.inbounds[i.ID] = &managed{instance: i, fingerprint: fp, process: proc}
	logger.Infof("%s: started local PPP daemon for inbound %d on TCP %d (%d client(s))", i.Protocol, i.ID, i.Port, len(i.Credentials))
	return nil
}

// RemoveChecked retains the managed process and configuration when its exit
// cannot be observed. The signal says nothing about child PPP sessions.
func (m *Manager) RemoveChecked(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.checkUntrackedRuntimeLocked(); err != nil {
		return err
	}
	cur := m.inbounds[id]
	if cur == nil {
		return nil
	}
	if cur.process != nil {
		if err := cur.process.Stop(); err != nil {
			return fmt.Errorf("stop %s daemon for inbound %d: %w", cur.instance.Protocol, id, err)
		}
	}
	networkManager().removeInstance(cur.instance)
	removeChapSecrets(cur.instance)
	delete(m.inbounds, id)
	_ = removeInstanceDir(cur.instance)
	return nil
}

func (m *Manager) Remove(id int) {
	if err := m.RemoveChecked(id); err != nil {
		logger.Warningf("pppvpn: remove inbound %d failed: %v", id, err)
	}
}

func (m *Manager) Reconcile(desired []Instance) {
	m.mu.Lock()
	if m.manualStop {
		m.mu.Unlock()
		return
	}
	wanted := make(map[int]struct{}, len(desired))
	for _, i := range desired {
		wanted[i.ID] = struct{}{}
	}
	ids := make([]int, 0)
	for id := range m.inbounds {
		if _, ok := wanted[id]; !ok {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Remove(id)
	}
	for _, i := range desired {
		if err := m.Ensure(i); err != nil {
			m.mu.Lock()
			cur := m.inbounds[i.ID]
			shouldLog := cur == nil || time.Since(cur.lastErr) > time.Minute
			if cur == nil {
				m.inbounds[i.ID] = &managed{instance: i, lastErr: time.Now()}
			} else if shouldLog {
				cur.lastErr = time.Now()
			}
			m.mu.Unlock()
			if shouldLog {
				logger.Warningf("%s: reconcile inbound %d failed: %v", i.Protocol, i.ID, err)
			}
		}
	}
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]int, 0, len(m.inbounds))
	for id := range m.inbounds {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.Remove(id)
	}
}

// StopManually records a dashboard stop request. The reconcile job must not
// recreate PPP daemons until Resume or Restart clears the flag.
func (m *Manager) StopManually() error {
	m.mu.Lock()
	m.manualStop = true
	m.lastError = ""
	m.mu.Unlock()
	m.StopAll()
	return nil
}

// Resume clears a dashboard stop request; the next reconcile tick starts the
// enabled SSTP/PPTP inbounds again from the database's desired state.
func (m *Manager) Resume() {
	m.mu.Lock()
	m.manualStop = false
	m.lastError = ""
	m.mu.Unlock()
}

// Restart stops current daemons without leaving a manual-stop flag, so the
// following reconcile pass rebuilds them from current settings.
func (m *Manager) Restart() error {
	m.mu.Lock()
	m.manualStop = false
	m.lastError = ""
	m.mu.Unlock()
	m.StopAll()
	return nil
}

// Status reports only process-level state; traffic counters and quota changes
// continue to be owned by the shared accounting pipeline.
func (m *Manager) Status() RuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := RuntimeStatus{ManualStop: m.manualStop, Error: m.lastError}
	for _, cur := range m.inbounds {
		if cur.process != nil && cur.process.IsRunning() {
			status.Running = true
			break
		}
	}
	return status
}

func IsAvailable(protocol Protocol) bool { _, err := daemonPath(protocol); return err == nil }

func ValidateRuntimeConfig(i Instance) error {
	if _, err := daemonPath(i.Protocol); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Dir(dirFor(i))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
