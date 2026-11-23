package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/l2tp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// This local marker deliberately stays separate from the Xray journal: a
// suspected L2TP orphan must not prevent unrelated traffic polls from running.
// It supplements the shared database incident but is not a quiescence fence.
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

// Store each distinct observation separately so an earlier orphan incident
// does not erase the counter values needed for manual reconciliation.
func persistL2TPCounterEvidence(detail string) error {
	return persistAccountingEvidence("l2tp-accounting-evidence", detail)
}

func persistAccountingEvidence(dirName, detail string) error {
	dir := filepath.Join(config.GetDBFolderPath(), dirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := syncCounterIncidentDir(filepath.Dir(dir)); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(detail))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+".txt")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		// An incomplete earlier file remains evidence, but is not a durable
		// copy of this observation and must not release the poll marker.
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if string(data) != detail+"\n" {
			return errors.New("incomplete counter evidence requires manual review")
		}
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
		return syncCounterIncidentDir(dir)
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
	return syncCounterIncidentDir(dir)
}

// RecordL2TPCounterDecreaseUnderAllocationLock must only be called with the
// allocation lock already held and the poll marker committed. Failed or
// ambiguous persistence must leave that marker in place for manual review.
func RecordL2TPCounterDecreaseUnderAllocationLock(decrease *l2tp.CounterDecrease) error {
	if decrease == nil {
		return errors.New("missing L2TP counter observation")
	}
	l2tpIncidentPending.Store(true)
	evidenceErr := persistL2TPCounterEvidence(decrease.Error())
	markerErr := persistL2TPAccountingIncident("L2TP counter decrease requires reconciliation")
	db := database.GetDB()
	var dbErr error
	if db == nil {
		dbErr = errors.New("database unavailable")
	} else {
		incident := model.ResellerNodeCounterIncident{NodeId: -2, Email: "", Direction: "l2tp-counter-decreased"}
		dbErr = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error
	}
	if err := errors.Join(evidenceErr, markerErr, dbErr); err != nil {
		return err
	}
	l2tpIncidentPending.Store(false)
	return nil
}

// ObserveL2TPAccountingIncident retains a local warning on disk as soon as an
// untracked runtime is detected, even if its directory is later removed. A
// failed write remains latched in this process and is retried on later checks.
// Callers must not treat this as proof about other hosts or stopped sessions.
func ObserveL2TPAccountingIncident() error {
	evidenceDir := filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-evidence")
	entries, err := os.ReadDir(evidenceDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		l2tpIncidentPending.Store(true)
		return fmt.Errorf("inspect L2TP accounting evidence: %w", err)
	}
	if len(entries) > 0 {
		return errors.New("L2TP accounting evidence requires manual reconciliation")
	}
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

// checkPPPAndL2TPAccountingIncidents is used by sensitive operations, including inside
// writer transactions. It never tries to insert the shared incident in the
// caller's transaction: rolling that transaction back would erase evidence.
func checkPPPAndL2TPAccountingIncidents(db *gorm.DB) error {
	if err := checkPPPAccountingIncident(db); err != nil {
		return err
	}
	if err := ObserveL2TPAccountingIncident(); err != nil {
		return err
	}
	if db == nil {
		return errors.New("database unavailable while checking L2TP accounting incidents")
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&count).Error; err != nil {
		return fmt.Errorf("check shared L2TP accounting incident: %w", err)
	}
	if count > 0 {
		return errors.New("shared L2TP accounting incident requires manual reconciliation")
	}
	return nil
}

// PublishL2TPAccountingIncident runs outside traffic transactions. A committed
// shared record fences subsequent participating allocation operations on hosts
// using the same database; the local file remains if the insert or lock fails.
// The global lock serializes publication with participating mutations, but is
// not a lease or evidence that no writer ran before the incident was observed.
func PublishL2TPAccountingIncident() error {
	if err := ObserveL2TPAccountingIncident(); err == nil {
		return nil
	}
	unlock, err := LockResellerAllocationForXrayRecovery()
	if err != nil {
		return fmt.Errorf("lock L2TP incident publication: %w", err)
	}
	defer unlock()
	if err := ObserveL2TPAccountingIncident(); err == nil {
		return nil
	}
	db := database.GetDB()
	if db == nil {
		return errors.New("database unavailable while publishing L2TP incident")
	}
	incident := model.ResellerNodeCounterIncident{NodeId: -2, Email: "", Direction: "l2tp-orphan"}
	if err := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error; err != nil {
		return fmt.Errorf("persist shared L2TP accounting incident: %w", err)
	}
	return errors.New("L2TP accounting incident requires manual reconciliation")
}
