package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
)

// localXrayFallback is immutable evidence created by storeCounterIncidentFallback.
// Keeping its stat metadata lets reboot recovery distinguish evidence written
// before a new managed core from a new incident observed by that core.
type localXrayFallback struct {
	name     string
	incident model.ResellerNodeCounterIncident
	modTime  time.Time
}

func listLocalXrayFallbacks() ([]localXrayFallback, error) {
	entries, err := os.ReadDir(counterIncidentDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect counter incident fallback: %w", err)
	}
	fallbacks := make([]localXrayFallback, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("unexpected directory in counter incident fallback: %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect counter incident fallback %q: %w", entry.Name(), err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("unexpected counter incident fallback entry: %q", entry.Name())
		}
		data, err := os.ReadFile(filepath.Join(counterIncidentDir(), entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read counter incident fallback %q: %w", entry.Name(), err)
		}
		var incident model.ResellerNodeCounterIncident
		if err := json.Unmarshal(data, &incident); err != nil {
			return nil, fmt.Errorf("decode counter incident fallback %q: %w", entry.Name(), err)
		}
		if incident.NodeId == 0 {
			fallbacks = append(fallbacks, localXrayFallback{name: entry.Name(), incident: incident, modTime: info.ModTime()})
		}
	}
	sort.Slice(fallbacks, func(i, j int) bool { return fallbacks[i].name < fallbacks[j].name })
	return fallbacks, nil
}

func listLocalXrayCounterIncidents(db *gorm.DB) ([]model.ResellerNodeCounterIncident, error) {
	if db == nil {
		return nil, errors.New("database unavailable while checking Xray counter incidents")
	}
	var incidents []model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ?", 0).Order("id ASC").Find(&incidents).Error; err != nil {
		return nil, fmt.Errorf("read local Xray counter incidents: %w", err)
	}
	return incidents, nil
}

func localXrayCounterIncidentPresent(db *gorm.DB) (bool, error) {
	fallbacks, err := listLocalXrayFallbacks()
	if err != nil {
		return false, err
	}
	incidents, err := listLocalXrayCounterIncidents(db)
	if err != nil {
		return false, err
	}
	return len(fallbacks) != 0 || len(incidents) != 0, nil
}

// localXrayCounterIncidentRecoveryEligible is a non-mutating preflight. The
// caller must still repeat these checks immediately before archiving, because
// an incident can be recorded while the fresh baseline is being queried.
func localXrayCounterIncidentRecoveryEligible(db *gorm.DB, coreStartedAt time.Time) (present bool, eligible bool, err error) {
	fallbacks, err := listLocalXrayFallbacks()
	if err != nil {
		return false, false, err
	}
	incidents, err := listLocalXrayCounterIncidents(db)
	if err != nil {
		return false, false, err
	}
	present = len(fallbacks) != 0 || len(incidents) != 0
	if !present || coreStartedAt.IsZero() || db.Dialector.Name() != "sqlite" {
		return present, false, nil
	}
	for _, fallback := range fallbacks {
		if !fallback.modTime.Before(coreStartedAt) {
			return present, false, nil
		}
	}
	for _, incident := range incidents {
		if incident.CreatedAt != 0 && incident.CreatedAt >= coreStartedAt.UnixMilli() {
			return present, false, nil
		}
	}
	return present, true, nil
}

// recoverLocalXrayCounterIncidentsAfterRestart archives only local-Xray
// evidence that predates the currently managed core, after the caller has
// taken a fresh no-charge baseline from that core. The unknown prior interval
// is never converted to zero traffic: the original fallback and database rows
// are retained in the returned backup directory before their live barriers are
// removed.
//
// The operation is intentionally limited to SQLite. PostgreSQL can be shared
// by independently running panels, and a local process cannot prove that a
// shared incident belongs to an older local core. Those installations retain
// the existing explicit reconciliation flow instead of risking another host's
// active evidence.
func recoverLocalXrayCounterIncidentsAfterRestart(db *gorm.DB, coreStartedAt time.Time) (present bool, recovered bool, backupDir string, err error) {
	fallbacks, err := listLocalXrayFallbacks()
	if err != nil {
		return false, false, "", err
	}
	incidents, err := listLocalXrayCounterIncidents(db)
	if err != nil {
		return false, false, "", err
	}
	present = len(fallbacks) != 0 || len(incidents) != 0
	if !present {
		return false, false, "", nil
	}
	if coreStartedAt.IsZero() || db.Dialector.Name() != "sqlite" {
		return present, false, "", nil
	}
	// Evidence written by the current core is a live accounting incident, not
	// a reboot residue. Leave it fail-closed for the explicit repair workflow.
	for _, fallback := range fallbacks {
		if !fallback.modTime.Before(coreStartedAt) {
			return present, false, "", nil
		}
	}
	for _, incident := range incidents {
		if incident.CreatedAt != 0 && incident.CreatedAt >= coreStartedAt.UnixMilli() {
			return present, false, "", nil
		}
	}

	backupRoot := filepath.Join(config.GetDBFolderPath(), "reconciliation-backups")
	if err := os.MkdirAll(backupRoot, 0700); err != nil {
		return present, false, "", fmt.Errorf("create Xray recovery backup root: %w", err)
	}
	backupDir, err = os.MkdirTemp(backupRoot, "auto-xray-restart-")
	if err != nil {
		return present, false, "", fmt.Errorf("create Xray recovery backup: %w", err)
	}

	manifest := struct {
		Reason        string                              `json:"reason"`
		CoreStartedAt int64                               `json:"coreStartedAt"`
		FallbackFiles []string                            `json:"fallbackFiles"`
		Incidents     []model.ResellerNodeCounterIncident `json:"incidents"`
	}{
		Reason:        "fresh Xray baseline after restart; previous interval remains unknown",
		CoreStartedAt: coreStartedAt.UnixMilli(),
		Incidents:     incidents,
	}
	for _, fallback := range fallbacks {
		manifest.FallbackFiles = append(manifest.FallbackFiles, fallback.name)
	}
	if err := writeReconciliationBackup(backupDir, "recovery-manifest.json", manifest); err != nil {
		return present, false, backupDir, fmt.Errorf("write Xray recovery manifest: %w", err)
	}
	if err := writeReconciliationBackup(backupDir, "counter-incidents.json", incidents); err != nil {
		return present, false, backupDir, fmt.Errorf("back up local Xray incidents: %w", err)
	}

	if len(fallbacks) > 0 {
		fallbackBackup := filepath.Join(backupDir, "counter-incident-fallback")
		if err := os.MkdirAll(fallbackBackup, 0700); err != nil {
			return present, false, backupDir, fmt.Errorf("prepare Xray fallback archive: %w", err)
		}
		for _, fallback := range fallbacks {
			source := filepath.Join(counterIncidentDir(), fallback.name)
			target := filepath.Join(fallbackBackup, fallback.name)
			if err := os.Rename(source, target); err != nil {
				return present, false, backupDir, fmt.Errorf("archive local Xray fallback %q: %w", fallback.name, err)
			}
		}
		if err := syncCounterIncidentDir(counterIncidentDir()); err != nil {
			return present, false, backupDir, fmt.Errorf("sync Xray fallback archive: %w", err)
		}
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
				return fmt.Errorf("local Xray incident changed during recovery")
			}
			return nil
		}); err != nil {
			return present, false, backupDir, fmt.Errorf("clear archived local Xray incidents: %w", err)
		}
	}
	return present, true, backupDir, nil
}
