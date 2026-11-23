package job

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/google/uuid"
)

// The manager is process-global, so every TuicJob shares this state. Call only
// under tuicJournalMu. Ordinary failures release the allocation lock while the
// durable marker continues to fence mutations; uncertain marker deletion keeps
// the lock until the fence is restored.
type tuicPollState struct {
	unlock          func()
	batch           *tuicTrafficBatch
	committed       bool
	removed         bool
	deleteUncertain bool
}

var sharedTuicPoll tuicPollState

func collectAndCommitTuic(state *tuicPollState, collect func() []tuic.ClientTrafficDelta, inbound *service.InboundService) ([]tuic.ClientTrafficDelta, bool, bool, error) {
	if state.batch == nil {
		unlock, err := service.LockResellerAllocation()
		if err != nil {
			return nil, false, false, err
		}
		id := uuid.NewString()
		if err := service.BeginXrayPoll(id); err != nil {
			unlock()
			return nil, false, false, fmt.Errorf("create TUIC poll marker: %w", err)
		}
		state.unlock = unlock
		state.batch = &tuicTrafficBatch{ID: id, Deltas: collect(), PollMarker: true}
	} else if state.unlock == nil {
		unlock, err := service.LockResellerAllocationForXrayRecovery()
		if err != nil {
			return nil, false, false, fmt.Errorf("reacquire TUIC poll lock: %w", err)
		}
		if err := service.VerifyExclusiveXrayPollMarker(state.batch.ID); err != nil {
			unlock()
			return nil, false, false, fmt.Errorf("verify TUIC poll marker: %w", err)
		}
		state.unlock = unlock
	}
	// A retained marker fences other writers while ordinary failures wait for
	// retry. Do not release after an uncertain marker delete: it may be gone.
	defer func() {
		if state.unlock != nil && !state.deleteUncertain {
			state.unlock()
			state.unlock = nil
		}
	}()
	batch := state.batch
	if state.deleteUncertain {
		if err := service.EnsureXrayPollMarker(batch.ID); err != nil {
			return nil, false, false, fmt.Errorf("restore uncertain TUIC poll marker: %w", err)
		}
		state.deleteUncertain = false
	}
	if !state.committed {
		if _, err := storeTuicBatch(*batch); err != nil {
			return nil, false, false, fmt.Errorf("journal TUIC poll: %w", err)
		}
		if err := inbound.AddTuicTrafficBatch(batch.ID, batch.Deltas); err != nil {
			return nil, false, false, fmt.Errorf("commit TUIC poll: %w", err)
		}
		state.committed = true
	}
	// Preserve the fence until quota enforcement has been attempted after the
	// accounting commit. Automatic renewal is deferred while this lock is held.
	restart, disabled, err := inbound.AddTraffic(nil, nil)
	if err != nil {
		return nil, restart, disabled, fmt.Errorf("enforce TUIC quota: %w", err)
	}
	if !state.removed {
		if err := os.Remove(filepath.Join(tuicJournalDir(), batch.ID+".json")); err != nil {
			return nil, restart, disabled, fmt.Errorf("remove committed TUIC journal: %w", err)
		}
		state.removed = true
	}
	if err := syncJournalDir(tuicJournalDir()); err != nil {
		return nil, restart, disabled, fmt.Errorf("sync TUIC journal removal: %w", err)
	}
	if err := service.FinishXrayPoll(batch.ID); err != nil {
		state.deleteUncertain = true
		return nil, restart, disabled, fmt.Errorf("clear TUIC poll marker: %w", err)
	}
	deltas := batch.Deltas
	state.unlock()
	*state = tuicPollState{}
	return deltas, restart, disabled, nil
}
