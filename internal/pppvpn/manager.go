package pppvpn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	mu       sync.Mutex
	inbounds map[int]*managed
}

var managerOnce sync.Once
var singleton *Manager

func GetManager() *Manager {
	managerOnce.Do(func() { singleton = &Manager{inbounds: map[int]*managed{}} })
	return singleton
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
		networkManager().removeInstance(i)
		removeChapSecrets(i)
		_ = removeInstanceDir(i)
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
