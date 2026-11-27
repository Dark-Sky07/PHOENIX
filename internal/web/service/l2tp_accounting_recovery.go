package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/l2tp"
	"gorm.io/gorm"
)

type l2tpRecoveryFile struct {
	name    string
	modTime time.Time
}

func l2tpRecoveryFiles(dir string) ([]l2tpRecoveryFile, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	files := make([]l2tpRecoveryFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("unexpected directory in L2TP accounting evidence: %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("unexpected L2TP accounting evidence entry: %q", entry.Name())
		}
		files = append(files, l2tpRecoveryFile{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

// l2tpRestartRecoveryEligible accepts only local marker/evidence that predates
// the currently running daemon epoch. A newly observed dropped session remains
// fail-closed; it cannot be released merely because a later poll happened to
// look healthy.
func l2tpRestartRecoveryEligible(startedAt time.Time) (bool, error) {
	if startedAt.IsZero() || l2tpIncidentPending.Load() {
		return false, nil
	}
	markerInfo, err := os.Stat(l2tpAccountingIncidentPath())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect L2TP accounting incident: %w", err)
	}
	if !markerInfo.Mode().IsRegular() {
		return false, errors.New("invalid L2TP accounting incident entry")
	}
	if !markerInfo.ModTime().Before(startedAt) {
		return false, nil
	}
	files, err := l2tpRecoveryFiles(filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-evidence"))
	if err != nil {
		return false, fmt.Errorf("inspect L2TP accounting evidence: %w", err)
	}
	for _, file := range files {
		if !file.modTime.Before(startedAt) {
			return false, nil
		}
	}
	return true, nil
}

// RecoverL2TPAccountingAfterRestart archives a marker left before the current
// L2TP daemon started. It must be called only after CollectTrafficChecked has
// completed successfully for that running daemon. No traffic amount is
// invented for the prior interval; the copied marker and incident rows remain
// in reconciliation-backups for operator audit.
//
// Shared PostgreSQL databases intentionally do not use this automatic release:
// a local process cannot establish that a shared -2 incident came only from an
// older local daemon. Operators can use the existing backup-first repair flow.
func RecoverL2TPAccountingAfterRestart(manager *l2tp.Manager) (bool, string, error) {
	if manager == nil {
		return false, "", errors.New("L2TP manager is unavailable")
	}
	startedAt := manager.StartedAt()
	eligible, err := l2tpRestartRecoveryEligible(startedAt)
	if err != nil || !eligible {
		return false, "", err
	}
	if err := manager.CheckUntrackedRuntime(); err != nil {
		return false, "", fmt.Errorf("L2TP runtime is not clean enough for restart recovery: %w", err)
	}
	db := database.GetDB()
	if db == nil {
		return false, "", errors.New("database unavailable while recovering L2TP accounting")
	}
	if db.Dialector.Name() != "sqlite" {
		return false, "", nil
	}

	// Recheck all local conditions after acquiring the cross-operation lock.
	unlock, err := LockResellerAllocationForXrayRecovery()
	if err != nil {
		return false, "", fmt.Errorf("lock L2TP restart recovery: %w", err)
	}
	defer unlock()
	eligible, err = l2tpRestartRecoveryEligible(startedAt)
	if err != nil || !eligible {
		return false, "", err
	}
	if err := manager.CheckUntrackedRuntime(); err != nil {
		return false, "", fmt.Errorf("L2TP runtime changed during restart recovery: %w", err)
	}

	evidenceDir := filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-evidence")
	evidenceFiles, err := l2tpRecoveryFiles(evidenceDir)
	if err != nil {
		return false, "", fmt.Errorf("list L2TP accounting evidence: %w", err)
	}
	var incidents []model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ?", -2).Order("id ASC").Find(&incidents).Error; err != nil {
		return false, "", fmt.Errorf("read shared L2TP accounting incident: %w", err)
	}

	backupRoot := filepath.Join(config.GetDBFolderPath(), "reconciliation-backups")
	if err := os.MkdirAll(backupRoot, 0700); err != nil {
		return false, "", fmt.Errorf("create L2TP recovery backup root: %w", err)
	}
	backupDir, err := os.MkdirTemp(backupRoot, "auto-l2tp-restart-")
	if err != nil {
		return false, "", fmt.Errorf("create L2TP recovery backup: %w", err)
	}
	manifest := struct {
		Reason        string                              `json:"reason"`
		DaemonStarted int64                               `json:"daemonStarted"`
		EvidenceFiles []string                            `json:"evidenceFiles"`
		Incidents     []model.ResellerNodeCounterIncident `json:"incidents"`
	}{
		Reason:        "healthy sample from replacement L2TP daemon; prior interval remains unknown",
		DaemonStarted: startedAt.UnixMilli(),
		Incidents:     incidents,
	}
	for _, evidence := range evidenceFiles {
		manifest.EvidenceFiles = append(manifest.EvidenceFiles, evidence.name)
	}
	if err := writeReconciliationBackup(backupDir, "recovery-manifest.json", manifest); err != nil {
		return false, backupDir, fmt.Errorf("write L2TP recovery manifest: %w", err)
	}
	if err := writeReconciliationBackup(backupDir, "counter-incidents.json", incidents); err != nil {
		return false, backupDir, fmt.Errorf("back up shared L2TP incident: %w", err)
	}

	markerBackup := filepath.Join(backupDir, "l2tp-accounting-incident")
	if err := os.Rename(l2tpAccountingIncidentPath(), markerBackup); err != nil {
		return false, backupDir, fmt.Errorf("archive L2TP accounting marker: %w", err)
	}
	if len(evidenceFiles) > 0 {
		evidenceBackup := filepath.Join(backupDir, "l2tp-accounting-evidence")
		if err := os.MkdirAll(evidenceBackup, 0700); err != nil {
			return false, backupDir, fmt.Errorf("prepare L2TP evidence archive: %w", err)
		}
		for _, evidence := range evidenceFiles {
			if err := os.Rename(filepath.Join(evidenceDir, evidence.name), filepath.Join(evidenceBackup, evidence.name)); err != nil {
				return false, backupDir, fmt.Errorf("archive L2TP evidence %q: %w", evidence.name, err)
			}
		}
		if err := syncCounterIncidentDir(evidenceDir); err != nil {
			return false, backupDir, fmt.Errorf("sync L2TP evidence archive: %w", err)
		}
	}
	if err := syncCounterIncidentDir(filepath.Dir(l2tpAccountingIncidentPath())); err != nil {
		return false, backupDir, fmt.Errorf("sync L2TP marker archive: %w", err)
	}

	if len(incidents) > 0 {
		ids := make([]int, 0, len(incidents))
		for _, incident := range incidents {
			ids = append(ids, incident.Id)
		}
		if err := db.Transaction(func(tx *gorm.DB) error {
			result := tx.Where("id IN ?", ids).Delete(&model.ResellerNodeCounterIncident{})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != int64(len(ids)) {
				return errors.New("shared L2TP incident changed during recovery")
			}
			return nil
		}); err != nil {
			return false, backupDir, fmt.Errorf("clear archived shared L2TP incident: %w", err)
		}
	}
	return true, backupDir, nil
}
