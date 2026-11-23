package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/l2tp"
)

// This local marker deliberately stays separate from the Xray journal: a
// suspected L2TP orphan must not prevent unrelated traffic polls from running.
// It is not a substitute for a cross-host incident or a quiescence fence.
var l2tpIncidentPending atomic.Bool

func l2tpAccountingIncidentPath() string {
	return filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-incident")
}

func persistL2TPAccountingIncident(detail string) error {
	path := l2tpAccountingIncidentPath()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		// A previous partial write still blocks; sync the existing evidence
		// before considering an in-memory persistence failure resolved.
		f, err = os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		if closeErr := f.Close(); syncErr == nil {
			syncErr = closeErr
		}
		if syncErr != nil {
			return syncErr
		}
		return syncCounterIncidentDir(filepath.Dir(path))
	}
	if err != nil {
		return err
	}
	_, writeErr := f.WriteString(detail + "\n")
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	return syncCounterIncidentDir(filepath.Dir(path))
}

// ObserveL2TPAccountingIncident retains a local warning on disk as soon as an
// untracked runtime is detected, even if its directory is later removed. A
// failed write remains latched in this process and is retried on later checks.
// Callers must not treat this as proof about other hosts or stopped sessions.
func ObserveL2TPAccountingIncident() error {
	path := l2tpAccountingIncidentPath()
	if _, err := os.Lstat(path); err == nil {
		if l2tpIncidentPending.Load() {
			if err := persistL2TPAccountingIncident(""); err != nil {
				return fmt.Errorf("L2TP accounting incident requires manual reconciliation; persist evidence: %w", err)
			}
			l2tpIncidentPending.Store(false)
		}
		return errors.New("L2TP accounting incident requires manual reconciliation")
	} else if !errors.Is(err, os.ErrNotExist) {
		// An unavailable incident store cannot establish that a prior warning
		// is absent. Remember the uncertainty if the store later recovers.
		l2tpIncidentPending.Store(true)
		return fmt.Errorf("inspect L2TP accounting incident: %w", err)
	}
	observation := l2tp.GetManager().CheckUntrackedRuntime()
	if observation == nil && !l2tpIncidentPending.Load() {
		return nil
	}
	l2tpIncidentPending.Store(true)
	detail := "L2TP accounting incident persistence was previously incomplete"
	if observation != nil {
		detail = observation.Error()
	}
	if err := persistL2TPAccountingIncident(detail); err != nil {
		return fmt.Errorf("L2TP accounting incident requires manual reconciliation; persist evidence: %w", err)
	}
	l2tpIncidentPending.Store(false)
	return errors.New("L2TP accounting incident requires manual reconciliation")
}
