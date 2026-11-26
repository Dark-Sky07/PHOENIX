package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

// ReconcileConfirmToken is required for a non-dry-run repair. A mistaken
// click on the UI button can therefore never remove evidence.
const ReconcileConfirmToken = "RECONCILE_COUNTER_INCIDENTS"

type ResellerClientMappingRow struct {
	ResellerID int    `json:"resellerId"`
	Username   string `json:"username,omitempty"`
	Email      string `json:"email"`
	CreatedAt  int64  `json:"createdAt,omitempty"`
}

type CounterIncidentRow struct {
	ID           int    `json:"id"`
	NodeID       int    `json:"nodeId"`
	Email        string `json:"email,omitempty"`
	Direction    string `json:"direction,omitempty"`
	PreviousUp   int64  `json:"previousUp,omitempty"`
	ObservedUp   int64  `json:"observedUp,omitempty"`
	PreviousDown int64  `json:"previousDown,omitempty"`
	ObservedDown int64  `json:"observedDown,omitempty"`
	CreatedAt    int64  `json:"createdAt,omitempty"`
}

type XrayAPIHealth struct {
	Port      int    `json:"port"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

// ReconciliationReport is an admin-only operational snapshot. It contains
// client identities because an operator must be able to identify and archive
// a dangling mapping; it is never exposed to resellers.
type ReconciliationReport struct {
	Available        bool                       `json:"available"`
	GeneratedAt      int64                      `json:"generatedAt"`
	CounterIncident  CounterIncidentStatus      `json:"counterIncident"`
	DanglingMappings []ResellerClientMappingRow `json:"danglingMappings"`
	DBIncidents      []CounterIncidentRow       `json:"dbIncidents"`
	InFlightPolls    []string                   `json:"inFlightPolls"`
	FallbackFiles    []string                   `json:"fallbackFiles"`
	XrayJournals     []string                   `json:"xrayJournals"`
	TuicJournals     []string                   `json:"tuicJournals"`
	XrayAPI          XrayAPIHealth              `json:"xrayApi"`
	Warnings         []string                   `json:"warnings"`
}

type ReconcileOptions struct {
	Confirm string `json:"confirm"`
	DryRun  bool   `json:"dryRun"`
}

type ReconcileResult struct {
	DryRun             bool     `json:"dryRun"`
	BackupDir          string   `json:"backupDir,omitempty"`
	ArchivedMappings   int64    `json:"archivedMappings"`
	ClearedIncidents   int64    `json:"clearedIncidents"`
	ClearedPolls       int64    `json:"clearedPolls"`
	MovedFallbackFiles int64    `json:"movedFallbackFiles"`
	Warnings           []string `json:"warnings"`
}

func listDanglingResellerClientMappings(db *gorm.DB) ([]ResellerClientMappingRow, error) {
	if db == nil {
		return nil, errors.New("database unavailable while checking reseller mappings")
	}
	var rows []ResellerClientMappingRow
	err := db.Table("reseller_clients AS rc").
		Select("rc.reseller_id AS reseller_id, COALESCE(r.username, '') AS username, rc.email AS email, rc.created_at AS created_at").
		Joins("LEFT JOIN resellers r ON r.id = rc.reseller_id").
		Joins("LEFT JOIN clients c ON LOWER(TRIM(c.email)) = LOWER(TRIM(rc.email))").
		Where("c.id IS NULL AND LOWER(TRIM(rc.email)) <> ''").
		Order("rc.reseller_id ASC, rc.email ASC").
		Scan(&rows).Error
	return rows, err
}

func listCounterIncidentRows(db *gorm.DB) ([]CounterIncidentRow, error) {
	if db == nil {
		return nil, errors.New("database unavailable while checking counter incidents")
	}
	var rows []CounterIncidentRow
	err := db.Model(&model.ResellerNodeCounterIncident{}).
		Select("id, node_id, email, direction, previous_up, observed_up, previous_down, observed_down, created_at").
		Order("id ASC").Scan(&rows).Error
	return rows, err
}

func listInFlightPolls(db *gorm.DB) ([]string, error) {
	if db == nil {
		return nil, errors.New("database unavailable while checking poll markers")
	}
	var ids []string
	err := db.Model(&model.XrayTrafficInFlight{}).Order("id ASC").Pluck("id", &ids).Error
	return ids, err
}

func listDirectoryEntries(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func readXrayAPIPort() (int, error) {
	data, err := os.ReadFile(xray.GetConfigPath())
	if err != nil {
		return 0, err
	}
	var cfg xray.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return 0, err
	}
	for _, inbound := range cfg.InboundConfigs {
		if inbound.Tag == "api" && inbound.Port > 0 {
			return inbound.Port, nil
		}
	}
	return 0, errors.New("Xray API inbound port not found")
}

func checkXrayAPIHealth() XrayAPIHealth {
	port, err := readXrayAPIPort()
	if err != nil {
		return XrayAPIHealth{Error: err.Error()}
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 700*time.Millisecond)
	if err != nil {
		return XrayAPIHealth{Port: port, Error: err.Error()}
	}
	_ = conn.Close()
	return XrayAPIHealth{Port: port, Reachable: true}
}

// ReadReconciliationReport collects every local evidence source that can gate
// reseller operations. Journal files are reported but never cleared by the
// API: they may contain unapplied traffic and require replay first.
func ReadReconciliationReport(db *gorm.DB) ReconciliationReport {
	report := ReconciliationReport{
		Available:   db != nil,
		GeneratedAt: time.Now().UnixMilli(),
		XrayAPI:     checkXrayAPIHealth(),
	}
	var err error
	if report.CounterIncident = readCounterIncidentStatus(db); !report.CounterIncident.Available {
		report.Warnings = append(report.Warnings, "Counter incident status is unavailable; treat the panel as degraded.")
	}
	if report.DanglingMappings, err = listDanglingResellerClientMappings(db); err != nil {
		report.Available = false
		report.Warnings = append(report.Warnings, "Could not read reseller client mappings: "+err.Error())
	}
	if report.DBIncidents, err = listCounterIncidentRows(db); err != nil {
		report.Available = false
		report.Warnings = append(report.Warnings, "Could not read counter incidents: "+err.Error())
	}
	if report.InFlightPolls, err = listInFlightPolls(db); err != nil {
		report.Available = false
		report.Warnings = append(report.Warnings, "Could not read poll markers: "+err.Error())
	}
	if report.FallbackFiles, err = listDirectoryEntries(counterIncidentDir()); err != nil {
		report.Available = false
		report.Warnings = append(report.Warnings, "Could not read counter incident fallback: "+err.Error())
	}
	if report.XrayJournals, err = listDirectoryEntries(XrayTrafficJournalDir()); err != nil {
		report.Available = false
		report.Warnings = append(report.Warnings, "Could not read Xray traffic journal: "+err.Error())
	}
	if report.TuicJournals, err = listDirectoryEntries(TuicTrafficJournalDir()); err != nil {
		report.Available = false
		report.Warnings = append(report.Warnings, "Could not read TUIC traffic journal: "+err.Error())
	}
	if !report.XrayAPI.Reachable && report.XrayAPI.Port > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("Xray API on 127.0.0.1:%d is unreachable.", report.XrayAPI.Port))
	}
	if len(report.XrayJournals) > 0 || len(report.TuicJournals) > 0 {
		report.Warnings = append(report.Warnings, "Traffic journals still hold unapplied usage; replay them before clearing their markers.")
	}
	return report
}

func writeReconciliationBackup(dir string, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0600)
}

// ReconcileCounterIncidents archives evidence first and only then removes
// markers. It never stops or restarts the panel or Xray, and it never deletes
// journal files, which may represent traffic that has not been applied yet.
func ReconcileCounterIncidents(db *gorm.DB, options ReconcileOptions) (ReconcileResult, error) {
	result := ReconcileResult{DryRun: true}
	if db == nil {
		return result, errors.New("database unavailable while reconciling")
	}
	if options.Confirm != ReconcileConfirmToken {
		result.Warnings = append(result.Warnings, "Dry run only: the confirmation token did not match; no evidence was changed.")
		return result, nil
	}
	if xrayCounterIncidentPending.Load() || xrayCoreExitPending.Load() {
		return result, errors.New("reconciliation refused while a local traffic incident is still being persisted")
	}

	report := ReadReconciliationReport(db)
	result.DryRun = options.DryRun
	if options.DryRun {
		result.ArchivedMappings = int64(len(report.DanglingMappings))
		result.ClearedIncidents = int64(len(report.DBIncidents))
		result.ClearedPolls = int64(len(report.InFlightPolls))
		result.MovedFallbackFiles = int64(len(report.FallbackFiles))
		result.Warnings = append(result.Warnings, "Dry run: no evidence was changed.")
		return result, nil
	}

	backupRoot := filepath.Join(config.GetDBFolderPath(), "reconciliation-backups", time.Now().UTC().Format("20060102-150405.000"))
	if err := os.MkdirAll(backupRoot, 0700); err != nil {
		return result, fmt.Errorf("create reconciliation backup: %w", err)
	}
	if err := writeReconciliationBackup(backupRoot, "report-before.json", report); err != nil {
		return result, fmt.Errorf("back up reconciliation report: %w", err)
	}
	result.BackupDir = backupRoot

	if len(report.FallbackFiles) > 0 {
		fallbackBackup := filepath.Join(backupRoot, "counter-incident-fallback")
		if err := os.MkdirAll(fallbackBackup, 0700); err != nil {
			return result, fmt.Errorf("prepare fallback backup: %w", err)
		}
		for _, name := range report.FallbackFiles {
			source := filepath.Join(counterIncidentDir(), name)
			if err := os.Rename(source, filepath.Join(fallbackBackup, name)); err != nil {
				return result, fmt.Errorf("move fallback evidence %q: %w", name, err)
			}
			result.MovedFallbackFiles++
		}
		if err := syncCounterIncidentDir(counterIncidentDir()); err != nil {
			return result, fmt.Errorf("sync fallback directory: %w", err)
		}
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		if len(report.DanglingMappings) > 0 {
			now := time.Now().UnixMilli()
			archives := make([]model.ResellerClientMappingArchive, 0, len(report.DanglingMappings))
			for _, row := range report.DanglingMappings {
				archives = append(archives, model.ResellerClientMappingArchive{
					ResellerID: row.ResellerID,
					Email:      row.Email,
					CreatedAt:  row.CreatedAt,
					ArchivedAt: now,
					Reason:     "no matching clients record",
					BackupPath: filepath.Join(backupRoot, "report-before.json"),
				})
			}
			if err := tx.Create(&archives).Error; err != nil {
				return fmt.Errorf("archive dangling mappings: %w", err)
			}
			keys := make([]string, 0, len(report.DanglingMappings))
			for _, row := range report.DanglingMappings {
				keys = append(keys, transferEmailKey(row.Email))
			}
			for _, batch := range chunkStrings(keys, sqlInChunk) {
				if err := tx.Exec(`DELETE FROM reseller_clients WHERE LOWER(TRIM(email)) IN ? AND NOT EXISTS (SELECT 1 FROM clients c WHERE LOWER(TRIM(c.email)) = LOWER(TRIM(reseller_clients.email)))`, batch).Error; err != nil {
					return fmt.Errorf("remove dangling mappings: %w", err)
				}
			}
			result.ArchivedMappings = int64(len(report.DanglingMappings))
		}
		if len(report.DBIncidents) > 0 {
			if err := writeReconciliationBackup(backupRoot, "counter-incidents.json", report.DBIncidents); err != nil {
				return fmt.Errorf("back up counter incidents: %w", err)
			}
			if err := tx.Where("1 = 1").Delete(&model.ResellerNodeCounterIncident{}).Error; err != nil {
				return fmt.Errorf("clear counter incidents: %w", err)
			}
			result.ClearedIncidents = int64(len(report.DBIncidents))
		}
		if len(report.InFlightPolls) > 0 {
			if err := writeReconciliationBackup(backupRoot, "in-flight-polls.json", report.InFlightPolls); err != nil {
				return fmt.Errorf("back up poll markers: %w", err)
			}
			if err := tx.Where("1 = 1").Delete(&model.XrayTrafficInFlight{}).Error; err != nil {
				return fmt.Errorf("clear poll markers: %w", err)
			}
			result.ClearedPolls = int64(len(report.InFlightPolls))
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	if len(report.XrayJournals) > 0 || len(report.TuicJournals) > 0 {
		result.Warnings = append(result.Warnings, "Traffic journals were not cleared; replay or review them before relying on usage numbers.")
	}
	result.Warnings = append(result.Warnings, "Verify the resellers list and online clients again; no service was stopped or restarted.")
	return result, nil
}

// ReconciliationNeeded reports whether a reseller view should show the warning
// banner. It is deliberately cheap enough for page loads.
func ReconciliationNeeded(report ReconciliationReport) bool {
	return !report.XrayAPI.Reachable ||
		len(report.DanglingMappings) > 0 ||
		len(report.DBIncidents) > 0 ||
		len(report.InFlightPolls) > 0 ||
		len(report.FallbackFiles) > 0 ||
		len(report.XrayJournals) > 0 ||
		len(report.TuicJournals) > 0
}
