package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var pppIncidentPending atomic.Bool
var pppIncidentWrites = struct {
	sync.Mutex
	pending map[string]bool
}{pending: make(map[string]bool)}

func notePPPIncidentWrite(key string, done bool, failed bool) {
	pppIncidentWrites.Lock()
	defer pppIncidentWrites.Unlock()
	if !done || failed {
		pppIncidentWrites.pending[key] = true
	} else {
		delete(pppIncidentWrites.pending, key)
	}
	pppIncidentPending.Store(len(pppIncidentWrites.pending) != 0)
}

const pppEvidenceDir = "ppp-accounting-evidence"

func localPPPAccountingIncident() error {
	if pppIncidentPending.Load() {
		return errors.New("PPP accounting incident persistence pending; reconciliation required")
	}
	entries, err := os.ReadDir(filepath.Join(config.GetDBFolderPath(), pppEvidenceDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect PPP accounting evidence: %w", err)
	}
	if len(entries) > 0 {
		return errors.New("PPP accounting evidence requires manual reconciliation")
	}
	return nil
}

func checkPPPAccountingIncident(db *gorm.DB) error {
	if err := localPPPAccountingIncident(); err != nil {
		return err
	}
	if db == nil {
		return errors.New("database unavailable while checking PPP accounting incidents")
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -3).Count(&count).Error; err != nil {
		return fmt.Errorf("check PPP accounting incidents: %w", err)
	}
	if count > 0 {
		return errors.New("shared PPP accounting incident requires manual reconciliation")
	}
	return nil
}

// recordPPPCounterDecrease is called only after the traffic transaction has
// ended. The evidence is kept locally even if acquiring the allocation lock or
// writing the shared record fails. No byte count is inferred from the error.
func recordPPPCounterDecrease(decrease *PPPCounterDecrease, allocationHeld bool) error {
	if decrease == nil {
		return errors.New("missing PPP counter observation")
	}
	detail := decrease.Error()
	notePPPIncidentWrite(detail, false, false)
	// A failed concurrent observation remains pending even when a different
	// observation succeeds. A successful retry clears only its own entry.
	completed := false
	defer func() { notePPPIncidentWrite(detail, true, !completed) }()
	fileErr := persistAccountingEvidence(pppEvidenceDir, detail)
	var unlock func()
	if !allocationHeld {
		var err error
		unlock, err = LockResellerAllocationForXrayRecovery()
		if err != nil {
			return errors.Join(fileErr, fmt.Errorf("lock PPP incident publication: %w", err))
		}
		defer unlock()
	}
	db := database.GetDB()
	var dbErr error
	if db == nil {
		dbErr = errors.New("database unavailable")
	} else {
		incident := model.ResellerNodeCounterIncident{NodeId: -3, Email: "", Direction: "ppp-counter-decreased"}
		dbErr = db.Clauses(clause.OnConflict{DoNothing: true}).Create(&incident).Error
	}
	if err := errors.Join(fileErr, dbErr); err != nil {
		return err
	}
	completed = true
	return nil
}
