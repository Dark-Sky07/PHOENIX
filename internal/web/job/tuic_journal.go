package job

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/web/websocket"
)

var tuicJournalMu sync.Mutex

type tuicTrafficBatch struct {
	ID         string                    `json:"id"`
	Deltas     []tuic.ClientTrafficDelta `json:"deltas"`
	PollMarker bool                      `json:"pollMarker,omitempty"`
}

func tuicJournalDir() string { return service.TuicTrafficJournalDir() }

func syncJournalDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// Accept existing legacy IDs without allowing them to escape the directory.
func validTuicBatchID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	return strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) == -1
}

func verifyStoredTuicBatch(dir, name string, wanted tuicTrafficBatch) error {
	data, err := readXrayJournal(dir, name)
	if err != nil {
		return err
	}
	var saved tuicTrafficBatch
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("invalid existing TUIC journal %q: %w", name, err)
	}
	if !reflect.DeepEqual(saved, wanted) {
		return fmt.Errorf("TUIC journal %q differs from retry payload; manual reconciliation required", name)
	}
	return nil
}

func storeTuicBatch(batch tuicTrafficBatch) (string, error) {
	if !validTuicBatchID(batch.ID) {
		return "", fmt.Errorf("invalid TUIC batch ID")
	}
	dir := tuicJournalDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := syncJournalDir(filepath.Dir(dir)); err != nil {
		return "", err
	}
	data, err := json.Marshal(batch)
	if err != nil {
		return "", err
	}
	if len(data) > maxXrayJournalBytes {
		return "", fmt.Errorf("TUIC batch exceeds journal size limit")
	}
	name := batch.ID + ".json"
	path := filepath.Join(dir, name)
	if err := verifyStoredTuicBatch(dir, name, batch); err == nil {
		return path, syncJournalDir(dir)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	file, err := os.CreateTemp(dir, ".pending-")
	if err != nil {
		return "", err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmp, path); err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		if err := verifyStoredTuicBatch(dir, name, batch); err != nil {
			return "", err
		}
	}
	// Once published, return the path even if cleanup or sync fails so the
	// caller does not also requeue the same deltas in memory.
	if err := os.Remove(tmp); err != nil {
		return path, err
	}
	return path, syncJournalDir(dir)
}

func (j *TuicJob) replayTuicJournal() error {
	entries, err := os.ReadDir(tuicJournalDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("unexpected TUIC journal entry %q; manual reconciliation required", entry.Name())
		}
		path := filepath.Join(tuicJournalDir(), entry.Name())
		data, err := readXrayJournal(tuicJournalDir(), entry.Name())
		if err != nil {
			return err
		}
		var batch tuicTrafficBatch
		if err := json.Unmarshal(data, &batch); err != nil {
			return fmt.Errorf("TUIC journal %s: %w", entry.Name(), err)
		}
		if !validTuicBatchID(batch.ID) || batch.ID+".json" != entry.Name() {
			return fmt.Errorf("TUIC journal batch ID mismatch")
		}
		if batch.PollMarker {
			if err := j.recoverCommittedTuicPoll(batch); err != nil {
				return fmt.Errorf("recover TUIC periodic journal %s: %w", entry.Name(), err)
			}
			continue
		}
		for _, delta := range batch.Deltas {
			if delta.Up < 0 || delta.Down < 0 {
				return fmt.Errorf("TUIC journal %s has negative traffic", entry.Name())
			}
		}
		if err := j.inboundService.AddTuicTrafficBatch(batch.ID, batch.Deltas); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := syncJournalDir(tuicJournalDir()); err != nil {
			return err
		}
	}
	return nil
}

// recoverCommittedTuicPoll only cleans up a journal if the shared receipt
// proves that this exact payload already committed. Uncommitted samples lack a
// durable ownership snapshot and must not be applied automatically on restart.
// The caller holds tuicJournalMu; on a later cleanup failure the same process
// retains state for retry and keeps the lock only for an uncertain deletion.
func (j *TuicJob) recoverCommittedTuicPoll(batch tuicTrafficBatch) error {
	if sharedTuicPoll.batch != nil {
		return fmt.Errorf("another TUIC poll is pending")
	}
	unlock, err := service.LockResellerAllocationForXrayRecovery()
	if err != nil {
		return err
	}
	if err := service.VerifyExclusiveXrayPollMarker(batch.ID); err != nil {
		unlock()
		return fmt.Errorf("verify TUIC poll marker: %w", err)
	}
	if err := service.VerifyCommittedTuicTrafficBatch(batch.ID, batch.Deltas); err != nil {
		unlock()
		return err
	}
	sharedTuicPoll = tuicPollState{unlock: unlock, batch: &batch, committed: true}
	_, _, disabled, err := collectAndCommitTuic(&sharedTuicPoll, nil, &j.inboundService)
	if disabled {
		websocket.BroadcastInvalidate(websocket.MessageTypeInbounds)
	}
	return err
}

func (j *TuicJob) flushTuicJournal() error {
	tuicJournalMu.Lock()
	defer tuicJournalMu.Unlock()
	if sharedTuicPoll.batch != nil {
		if _, _, _, err := collectAndCommitTuic(&sharedTuicPoll, nil, &j.inboundService); err != nil {
			return fmt.Errorf("finish pending TUIC poll before shutdown: %w", err)
		}
	}
	manager := tuic.GetManager()
	_, deltas := manager.CollectAllTraffic()
	if len(deltas) > 0 {
		if path, err := storeTuicBatch(tuicTrafficBatch{ID: uuid.NewString(), Deltas: deltas}); err != nil {
			if path == "" {
				manager.RequeueClientTraffic(deltas)
			}
			return fmt.Errorf("persist TUIC shutdown journal: %w", err)
		}
	}
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = j.replayTuicJournal(); err == nil {
			return nil
		}
		if attempt < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return fmt.Errorf("TUIC traffic retained in durable journal: %w", err)
}
