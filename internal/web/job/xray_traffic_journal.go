package job

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

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

func xrayJournalDir() string { return service.XrayTrafficJournalDir() }

func validXrayBatchID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

// Bound memory use on both creation and recovery; oversized snapshots remain
// pending instead of being truncated or applied without their full evidence.
const maxXrayJournalBytes = 64 << 20

func readXrayJournal(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := openXrayJournalRecord(root, name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxXrayJournalBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > maxXrayJournalBytes {
		return nil, fmt.Errorf("Xray journal %q exceeds size limit", name)
	}
	return data, nil
}

func verifyStoredXrayBatch(dir, name string, wanted xrayTrafficJournalEntry) error {
	data, err := readXrayJournal(dir, name)
	if err != nil {
		return err
	}
	var saved xrayTrafficJournalEntry
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("invalid existing Xray journal %q: %w", name, err)
	}
	if !reflect.DeepEqual(saved, wanted) {
		return fmt.Errorf("existing Xray journal %q differs from retry payload; manual reconciliation required", name)
	}
	return nil
}

// Unknown extensions already stop all Xray journal recovery, including on
// older binaries. OpenVPN batches remain retryable in this process, but a
// crash leaves their ownership/generation to manual reconciliation.
func xrayBatchJournalName(batch *xrayTrafficBatch) string {
	if batch.manual {
		return batch.id + ".manual"
	}
	return batch.id + ".json"
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
	// Compare the serialized representation, including legacy null/empty slice
	// distinctions, before using an existing ID for a retry. Never silently
	// apply different in-memory data and delete the original journal.
	name := xrayBatchJournalName(batch)
	path := filepath.Join(dir, name)
	data, err := json.Marshal(xrayTrafficJournalEntry{ID: batch.id, Traffic: batch.traffic, Clients: batch.clients, Identities: batch.identities})
	if err != nil {
		return err
	}
	if len(data) > maxXrayJournalBytes {
		return fmt.Errorf("Xray batch exceeds journal size limit")
	}
	var wanted xrayTrafficJournalEntry
	if err := json.Unmarshal(data, &wanted); err != nil {
		return err
	}
	if err := verifyStoredXrayBatch(dir, name, wanted); err == nil {
		return syncJournalDir(dir)
	} else if !os.IsNotExist(err) {
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
	// Publish without replacement: concurrent writers for one ID must compare
	// against the winner, not overwrite its durable evidence.
	if err := os.Link(tmp, path); err != nil {
		if !os.IsExist(err) {
			return err
		}
		if err := verifyStoredXrayBatch(dir, name, wanted); err != nil {
			return err
		}
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return syncJournalDir(dir)
}

// applyJournaledXrayBatch retains the transaction receipt even after journal
// removal. An independent host or restored backup may still hold a copy.
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
	if err := os.Remove(filepath.Join(xrayJournalDir(), xrayBatchJournalName(batch))); err != nil {
		return restart, disabled, err
	}
	if err := syncJournalDir(xrayJournalDir()); err != nil {
		return restart, disabled, err
	}
	// Keep the idempotency receipt until the durable poll marker is gone.
	// If marker deletion fails, a retry can recreate the journal without
	// charging the same batch again.
	if err := service.FinishXrayPoll(batch.id); err != nil {
		return restart, disabled, fmt.Errorf("clear Xray poll marker: %w", err)
	}
	// Do not discard the receipt just because this host removed its journal:
	// another host or backup can still replay the same UUID.
	return restart, disabled, nil
}

func (j *XrayTrafficJob) replayXrayJournal() error {
	unlock, err := service.LockResellerAllocationForXrayRecovery()
	if err != nil {
		return err
	}
	defer unlock()
	entries, err := os.ReadDir(xrayJournalDir())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("unexpected Xray journal entry %q; manual reconciliation required", entry.Name())
		}
		data, err := readXrayJournal(xrayJournalDir(), entry.Name())
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
	// A crash can occur after a committed batch and journal cleanup but
	// before its poll marker is removed. The receipt proves that batch was
	// applied; retain the receipt in case another host has the journal.
	return service.RecoverCommittedXrayPollMarkers()
}
