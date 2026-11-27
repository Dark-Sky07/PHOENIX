package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// reconciliationAfterBackupHook is used by the race-regression test to insert
// fresh evidence after the immutable backup report has been written. Production
// code never assigns it. The repair must preserve that later evidence instead
// of deleting every row in an evidence table.
var reconciliationAfterBackupHook func()

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
	Available         bool                       `json:"available"`
	GeneratedAt       int64                      `json:"generatedAt"`
	CounterIncident   CounterIncidentStatus      `json:"counterIncident"`
	DanglingMappings  []ResellerClientMappingRow `json:"danglingMappings"`
	DBIncidents       []CounterIncidentRow       `json:"dbIncidents"`
	InFlightPolls     []string                   `json:"inFlightPolls"`
	FallbackFiles     []string                   `json:"fallbackFiles"`
	L2TPMarkerPresent bool                       `json:"l2tpMarkerPresent"`
	L2TPEvidenceFiles []string                   `json:"l2tpEvidenceFiles"`
	XrayJournals      []string                   `json:"xrayJournals"`
	TuicJournals      []string                   `json:"tuicJournals"`
	XrayAPI           XrayAPIHealth              `json:"xrayApi"`
	Warnings          []string                   `json:"warnings"`
}

type ReconcileOptions struct {
	Confirm string `json:"confirm"`
	DryRun  bool   `json:"dryRun"`
}

type ReconcileResult struct {
	DryRun                 bool     `json:"dryRun"`
	BackupDir              string   `json:"backupDir,omitempty"`
	ArchivedMappings       int64    `json:"archivedMappings"`
	ClearedIncidents       int64    `json:"clearedIncidents"`
	ClearedPolls           int64    `json:"clearedPolls"`
	MovedFallbackFiles     int64    `json:"movedFallbackFiles"`
	MovedL2TPMarker        bool     `json:"movedL2tpMarker"`
	MovedL2TPEvidenceFiles int64    `json:"movedL2tpEvidenceFiles"`
	Warnings               []string `json:"warnings"`
}

// emptySlice keeps the public JSON schema stable. A nil Go slice serializes as
// null, while callers of these operational APIs need a collection they can
// safely enumerate even when there is no evidence to report.
func emptySlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}

func (report *ReconciliationReport) normalizeCollections() {
	report.DanglingMappings = emptySlice(report.DanglingMappings)
	report.DBIncidents = emptySlice(report.DBIncidents)
	report.InFlightPolls = emptySlice(report.InFlightPolls)
	report.FallbackFiles = emptySlice(report.FallbackFiles)
	report.L2TPEvidenceFiles = emptySlice(report.L2TPEvidenceFiles)
	report.XrayJournals = emptySlice(report.XrayJournals)
	report.TuicJournals = emptySlice(report.TuicJournals)
	report.Warnings = emptySlice(report.Warnings)
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

// listL2TPReconciliationEvidence reports the local half of an L2TP accounting
// incident. The shared database row alone is insufficient: leaving this marker
// or one of its captured observations in place makes every later L2TP poll
// fail closed. Reject unexpected filesystem entries rather than archiving an
// arbitrary directory or symlink from a privileged reconciliation endpoint.
func listL2TPReconciliationEvidence() (markerPresent bool, evidence []string, err error) {
	marker, err := os.Lstat(l2tpAccountingIncidentPath())
	if errors.Is(err, os.ErrNotExist) {
		markerPresent = false
	} else if err != nil {
		return false, nil, fmt.Errorf("inspect L2TP accounting marker: %w", err)
	} else if !marker.Mode().IsRegular() {
		return false, nil, errors.New("invalid L2TP accounting marker entry")
	} else {
		markerPresent = true
	}

	files, err := l2tpRecoveryFiles(filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-evidence"))
	if err != nil {
		return false, nil, fmt.Errorf("inspect L2TP accounting evidence: %w", err)
	}
	evidence = make([]string, 0, len(files))
	for _, file := range files {
		evidence = append(evidence, file.name)
	}
	return markerPresent, evidence, nil
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
	api := xray.XrayAPI{}
	if err := api.Init(port); err != nil {
		return XrayAPIHealth{Port: port, Error: err.Error()}
	}
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := api.HealthCheck(ctx); err != nil {
		return XrayAPIHealth{Port: port, Error: err.Error()}
	}
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
	if report.L2TPMarkerPresent, report.L2TPEvidenceFiles, err = listL2TPReconciliationEvidence(); err != nil {
		report.Available = false
		report.Warnings = append(report.Warnings, "Could not read L2TP accounting evidence: "+err.Error())
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
	report.normalizeCollections()
	return report
}

func writeReconciliationBackup(dir string, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), append(data, '\n'), 0600)
}

// archiveL2TPReconciliationEvidence moves exactly the files present in the
// reviewed report. A new observation is deliberately left live and prevents
// the matching database barrier from being cleared below.
func archiveL2TPReconciliationEvidence(report ReconciliationReport, backupDir string, result *ReconcileResult) error {
	if !report.L2TPMarkerPresent && len(report.L2TPEvidenceFiles) == 0 {
		return nil
	}
	moveRegular := func(source, target, label string) error {
		info, err := os.Lstat(source)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", label, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("invalid %s entry", label)
		}
		if err := os.Rename(source, target); err != nil {
			return fmt.Errorf("archive %s: %w", label, err)
		}
		return nil
	}
	if report.L2TPMarkerPresent {
		if err := moveRegular(l2tpAccountingIncidentPath(), filepath.Join(backupDir, "l2tp-accounting-incident"), "L2TP accounting marker"); err != nil {
			return fmt.Errorf("archive L2TP accounting marker: %w", err)
		}
		result.MovedL2TPMarker = true
	}

	evidenceDir := filepath.Join(config.GetDBFolderPath(), "l2tp-accounting-evidence")
	if len(report.L2TPEvidenceFiles) > 0 {
		evidenceBackup := filepath.Join(backupDir, "l2tp-accounting-evidence")
		if err := os.MkdirAll(evidenceBackup, 0700); err != nil {
			return fmt.Errorf("prepare L2TP accounting evidence backup: %w", err)
		}
		for _, name := range report.L2TPEvidenceFiles {
			if err := moveRegular(filepath.Join(evidenceDir, name), filepath.Join(evidenceBackup, name), fmt.Sprintf("L2TP accounting evidence %q", name)); err != nil {
				return err
			}
			result.MovedL2TPEvidenceFiles++
		}
		if err := syncCounterIncidentDir(evidenceDir); err != nil {
			return fmt.Errorf("sync L2TP accounting evidence archive: %w", err)
		}
	}
	if report.L2TPMarkerPresent {
		if err := syncCounterIncidentDir(filepath.Dir(l2tpAccountingIncidentPath())); err != nil {
			return fmt.Errorf("sync L2TP accounting marker archive: %w", err)
		}
	}
	if err := syncCounterIncidentDir(backupDir); err != nil {
		return fmt.Errorf("sync L2TP accounting backup: %w", err)
	}
	return nil
}

// ReconcileCounterIncidents archives evidence first and only then removes
// markers. It never stops or restarts the panel or Xray, and it never deletes
// journal files, which may represent traffic that has not been applied yet.
func ReconcileCounterIncidents(db *gorm.DB, options ReconcileOptions) (ReconcileResult, error) {
	result := ReconcileResult{DryRun: true, Warnings: []string{}}
	if db == nil {
		return result, errors.New("database unavailable while reconciling")
	}
	if options.Confirm != ReconcileConfirmToken {
		result.Warnings = append(result.Warnings, "Dry run only: the confirmation token did not match; no evidence was changed.")
		return result, nil
	}
	if xrayCounterIncidentPending.Load() || xrayCoreExitPending.Load() || xrayAPIOutagePending.Load() || l2tpIncidentPending.Load() {
		return result, errors.New("reconciliation refused while a local traffic incident is still being persisted")
	}

	report := ReadReconciliationReport(db)
	result.DryRun = options.DryRun
	if options.DryRun {
		result.ArchivedMappings = int64(len(report.DanglingMappings))
		result.ClearedIncidents = int64(len(report.DBIncidents))
		result.ClearedPolls = int64(len(report.InFlightPolls))
		result.MovedFallbackFiles = int64(len(report.FallbackFiles))
		result.MovedL2TPMarker = report.L2TPMarkerPresent
		result.MovedL2TPEvidenceFiles = int64(len(report.L2TPEvidenceFiles))
		result.Warnings = append(result.Warnings, "Dry run: no evidence was changed.")
		return result, nil
	}

	// Serialize with L2TP incident publication on this panel and every panel
	// deliberately sharing the active database. Re-read under that lock: an
	// operator must never clear a snapshot that changed while waiting to repair.
	unlock, err := LockResellerAllocationForXrayRecovery()
	if err != nil {
		return result, fmt.Errorf("lock reconciliation: %w", err)
	}
	defer unlock()
	if xrayCounterIncidentPending.Load() || xrayCoreExitPending.Load() || xrayAPIOutagePending.Load() || l2tpIncidentPending.Load() {
		return result, errors.New("reconciliation refused while a local traffic incident is still being persisted")
	}
	report = ReadReconciliationReport(db)
	if !report.Available {
		return result, errors.New("reconciliation refused because its evidence report is incomplete")
	}

	backupRoot := filepath.Join(config.GetDBFolderPath(), "reconciliation-backups", time.Now().UTC().Format("20060102-150405.000"))
	if err := os.MkdirAll(backupRoot, 0700); err != nil {
		return result, fmt.Errorf("create reconciliation backup: %w", err)
	}
	if err := writeReconciliationBackup(backupRoot, "report-before.json", report); err != nil {
		return result, fmt.Errorf("back up reconciliation report: %w", err)
	}
	result.BackupDir = backupRoot
	if reconciliationAfterBackupHook != nil {
		reconciliationAfterBackupHook()
	}

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
	if err := archiveL2TPReconciliationEvidence(report, backupRoot, &result); err != nil {
		return result, err
	}
	// A new L2TP observation can share the existing -2 database key. Do not
	// clear that row if fresh local evidence appeared while this archive ran:
	// the archived old file remains auditable and the newer evidence keeps the
	// panel safely fail-closed for a fresh operator review.
	markerPresent, evidenceFiles, err := listL2TPReconciliationEvidence()
	if err != nil {
		return result, err
	}
	if markerPresent || len(evidenceFiles) > 0 {
		return result, errors.New("L2TP accounting evidence changed during reconciliation; database incidents were retained")
	}

	err = db.Transaction(func(tx *gorm.DB) error {
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
			// Delete only the rows captured in report-before.json. A new
			// incident can be written by another panel process after this repair
			// began; clearing the entire table would silently discard that newer
			// evidence without a backup.
			incidentIDs := make([]int, 0, len(report.DBIncidents))
			for _, incident := range report.DBIncidents {
				incidentIDs = append(incidentIDs, incident.ID)
			}
			cleared := tx.Where("id IN ?", incidentIDs).Delete(&model.ResellerNodeCounterIncident{})
			if cleared.Error != nil {
				return fmt.Errorf("clear counter incidents: %w", cleared.Error)
			}
			result.ClearedIncidents = cleared.RowsAffected
		}
		if len(report.InFlightPolls) > 0 {
			if err := writeReconciliationBackup(backupRoot, "in-flight-polls.json", report.InFlightPolls); err != nil {
				return fmt.Errorf("back up poll markers: %w", err)
			}
			// The same snapshot rule applies to durable poll barriers. A marker
			// created after the report belongs to a later poll and must remain
			// until it has its own evidence and reconciliation decision.
			cleared := tx.Where("id IN ?", report.InFlightPolls).Delete(&model.XrayTrafficInFlight{})
			if cleared.Error != nil {
				return fmt.Errorf("clear poll markers: %w", cleared.Error)
			}
			result.ClearedPolls = cleared.RowsAffected
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
		report.L2TPMarkerPresent ||
		len(report.L2TPEvidenceFiles) > 0 ||
		len(report.XrayJournals) > 0 ||
		len(report.TuicJournals) > 0
}
