package job

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

// Xray counters are read without resetting them, but the process-local delta
// baseline advances during the poll. Journal the resulting batch before the
// database write so a failed write can be retried after a panel restart.
type xrayTrafficJournalEntry struct {
	ID         string                        `json:"id"`
	Traffic    []*xray.Traffic               `json:"traffic"`
	Clients    []*xray.ClientTraffic         `json:"clients"`
	Identities []service.XrayTrafficIdentity `json:"identities"`
}

func xrayJournalDir() string { return filepath.Join(config.GetDBFolderPath(), "xray-traffic-journal") }

func validXrayBatchID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

func storeXrayBatch(batch *xrayTrafficBatch) error {
	if batch == nil || !validXrayBatchID(batch.id) {
		return fmt.Errorf("invalid Xray batch ID")
	}
	dir := xrayJournalDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := syncJournalDir(filepath.Dir(dir)); err != nil {
		return err
	}
	// A retry must never replace its original on-disk payload. In particular,
	// the database code may normalize fields on the in-memory traffic objects.
	path := filepath.Join(dir, batch.id+".json")
	if _, err := os.Stat(path); err == nil {
		return syncJournalDir(dir)
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.Marshal(xrayTrafficJournalEntry{ID: batch.id, Traffic: batch.traffic, Clients: batch.clients, Identities: batch.identities})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncJournalDir(dir)
}

// applyJournaledXrayBatch retains the receipt until the removal of its journal
// file is durable. Otherwise a crash could replay a committed batch twice.
func (j *XrayTrafficJob) applyJournaledXrayBatch(batch *xrayTrafficBatch) (bool, bool, error) {
	if batch.identities == nil {
		identities, err := j.inboundService.CaptureXrayTrafficIdentities(batch.clients)
		if err != nil {
			return false, false, fmt.Errorf("capture Xray traffic identity: %w", err)
		}
		batch.identities = identities
	}
	if err := storeXrayBatch(batch); err != nil {
		return false, false, fmt.Errorf("journal Xray traffic: %w", err)
	}
	var restart, disabled bool
	var err error
	if j.captureUnlock != nil {
		restart, disabled, err = j.inboundService.AddCheckedXrayTrafficBatchUnderAllocationLock(batch.id, batch.traffic, batch.clients, batch.identities)
	} else {
		restart, disabled, err = j.inboundService.AddCheckedXrayTrafficBatch(batch.id, batch.traffic, batch.clients, batch.identities)
	}
	if err != nil {
		return restart, disabled, err
	}
	if err := os.Remove(filepath.Join(xrayJournalDir(), batch.id+".json")); err != nil {
		return restart, disabled, err
	}
	if err := syncJournalDir(xrayJournalDir()); err != nil {
		return restart, disabled, err
	}
	// An orphan receipt after a cleanup failure is harmless and cannot cause a
	// duplicate charge. A UUID is never reused for a different poll.
	if err := j.inboundService.ForgetXrayTrafficBatch(batch.id); err != nil {
		// No retry is needed: the corresponding journal removal is durable.
		logger.Warning("clean Xray traffic receipt failed:", err)
	}
	return restart, disabled, nil
}

func (j *XrayTrafficJob) replayXrayJournal() error {
	entries, err := os.ReadDir(xrayJournalDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(xrayJournalDir(), entry.Name()))
		if err != nil {
			return err
		}
		var saved xrayTrafficJournalEntry
		if err := json.Unmarshal(data, &saved); err != nil {
			return fmt.Errorf("Xray journal %s: %w", entry.Name(), err)
		}
		if !validXrayBatchID(saved.ID) || entry.Name() != saved.ID+".json" {
			return fmt.Errorf("invalid Xray journal batch ID")
		}
		if saved.Identities == nil {
			return fmt.Errorf("legacy Xray journal %s has no identity snapshot; manual reconciliation required", entry.Name())
		}
		batch := &xrayTrafficBatch{id: saved.ID, traffic: saved.Traffic, clients: saved.Clients, identities: saved.Identities}
		restart, disabled, err := j.applyJournaledXrayBatch(batch)
		if err != nil {
			return err
		}
		if restart || disabled {
			j.xrayService.SetToNeedRestart()
		}
	}
	return nil
}
