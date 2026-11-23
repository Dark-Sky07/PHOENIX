package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/pppvpn"
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
	observation := pppvpn.GetManager().CheckUntrackedRuntime()
	if observation == nil {
		observation = pppvpn.CheckHookIncidents()
	}
	if observation != nil {
		detail := observation.Error()
		notePPPIncidentWrite(detail, false, false)
		err := persistAccountingEvidence(pppEvidenceDir, detail)
		notePPPIncidentWrite(detail, true, err != nil)
		if err != nil {
			return fmt.Errorf("PPP runtime or hook incident requires manual reconciliation; persist evidence: %w", err)
		}
		return errors.New("PPP runtime or hook incident requires manual reconciliation")
	}
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

// ConfirmPPPCounterIncidentUnderAllocationLock checks both exact local evidence
// and the shared incident before a caller removes its durable poll marker.
// The caller must already hold the allocation lock.
func ConfirmPPPCounterIncidentUnderAllocationLock(decrease *PPPCounterDecrease) error {
	if decrease == nil || pppIncidentPending.Load() {
		return errors.New("PPP incident evidence is not durable")
	}
	detail := decrease.Error()
	sum := sha256.Sum256([]byte(detail))
	path := filepath.Join(config.GetDBFolderPath(), pppEvidenceDir, hex.EncodeToString(sum[:])+".txt")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(data) != detail+"\n" {
		return errors.New("PPP incident evidence is incomplete")
	}
	db := database.GetDB()
	if db == nil {
		return errors.New("database unavailable")
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -3).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return errors.New("shared PPP incident is missing")
	}
	return nil
}
