package job

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
)

func TestTuicPeriodicPollRetainsExactBatchAndFenceOnWriteFailure(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "periodic-tuic@example.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_periodic_tuic BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected'); END").Error; err != nil {
		t.Fatal(err)
	}
	state := &tuicPollState{}
	t.Cleanup(func() {
		if state.unlock != nil {
			state.unlock()
		}
	})
	calls := 0
	poll := func() []tuic.ClientTrafficDelta {
		calls++
		return []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 120, Down: 45}}
	}
	inbound := &service.InboundService{}
	if _, _, _, err := collectAndCommitTuic(state, poll, inbound); err == nil || state.batch == nil || state.unlock == nil {
		t.Fatalf("failed poll did not retain exact batch and lock: %v", err)
	}
	id := state.batch.ID
	if err := service.VerifyXrayPollMarker(id); err != nil {
		t.Fatalf("shared fence missing: %v", err)
	}
	path := filepath.Join(tuicJournalDir(), id+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("journal missing: %v", err)
	}
	var receipts int64
	if err := db.Model(&model.TuicTrafficReceipt{}).Where("id = ?", id).Count(&receipts).Error; err != nil || receipts != 0 {
		t.Fatalf("failed transaction committed receipt: %d %v", receipts, err)
	}
	if err := db.Exec("DROP TRIGGER reject_periodic_tuic").Error; err != nil {
		t.Fatal(err)
	}
	got, _, _, err := collectAndCommitTuic(state, poll, inbound)
	if err != nil || calls != 1 || len(got) != 1 || got[0].Up != 120 || state.batch != nil || state.unlock != nil {
		t.Fatalf("retry lost batch or polled twice: calls=%d got=%+v err=%v", calls, got, err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 120 || row.Down != 45 {
		t.Fatalf("usage incorrect: %+v %v", row, err)
	}
	if err := service.VerifyXrayPollMarker(id); err == nil {
		t.Fatal("completed poll marker remains")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("completed journal remains: %v", err)
	}
}

func TestTuicPeriodicMarkerDeleteFailureRestoresFenceOnRetry(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	if err := db.Exec("CREATE TRIGGER reject_tuic_marker_delete BEFORE DELETE ON xray_traffic_in_flights BEGIN SELECT RAISE(ABORT, 'injected'); END").Error; err != nil {
		t.Fatal(err)
	}
	state := &tuicPollState{}
	t.Cleanup(func() {
		if state.unlock != nil {
			state.unlock()
		}
	})
	calls := 0
	poll := func() []tuic.ClientTrafficDelta { calls++; return nil }
	if _, _, _, err := collectAndCommitTuic(state, poll, &service.InboundService{}); err == nil || !state.deleteUncertain || state.unlock == nil {
		t.Fatalf("failed marker deletion did not retain lock: %+v %v", state, err)
	}
	id := state.batch.ID
	// Simulate a deletion that committed despite returning an error; restore is
	// performed while continuously holding the original allocation lock.
	if err := db.Exec("DROP TRIGGER reject_tuic_marker_delete").Error; err != nil {
		t.Fatal(err)
	}
	if err := service.FinishXrayPoll(id); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := collectAndCommitTuic(state, poll, &service.InboundService{}); err != nil || calls != 1 || state.batch != nil {
		t.Fatalf("marker restoration retry failed: calls=%d err=%v", calls, err)
	}
	if err := service.VerifyXrayPollMarker(id); err == nil {
		t.Fatal("completed marker remains")
	}
}

func TestTuicCommittedPeriodicJournalRecoversAfterCrash(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "committed-tuic@example.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	batch := tuicTrafficBatch{ID: uuid.NewString(), PollMarker: true, Deltas: []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 33}}}
	if err := service.BeginXrayPoll(batch.ID); err != nil {
		t.Fatal(err)
	}
	path, err := storeTuicBatch(batch)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&service.InboundService{}).AddTuicTrafficBatch(batch.ID, batch.Deltas); err != nil {
		t.Fatal(err)
	}
	if err := NewTuicJob().replayTuicJournal(); err != nil {
		t.Fatalf("committed journal recovery: %v", err)
	}
	if err := db.First(&row, row.Id).Error; err != nil || row.Up != 33 {
		t.Fatalf("committed sample charged twice: %+v %v", row, err)
	}
	if err := service.VerifyXrayPollMarker(batch.ID); err == nil {
		t.Fatal("committed marker not cleared")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("committed journal not cleared: %v", err)
	}
}

func TestTuicPeriodicRecoveryRetainsLockOnUncertainMarkerDeletion(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	batch := tuicTrafficBatch{ID: uuid.NewString(), PollMarker: true}
	if err := service.BeginXrayPoll(batch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := storeTuicBatch(batch); err != nil {
		t.Fatal(err)
	}
	if err := (&service.InboundService{}).AddTuicTrafficBatch(batch.ID, batch.Deltas); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_recovery_tuic_marker BEFORE DELETE ON xray_traffic_in_flights BEGIN SELECT RAISE(ABORT, 'injected'); END").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sharedTuicPoll.unlock != nil {
			sharedTuicPoll.unlock()
		}
		sharedTuicPoll = tuicPollState{}
	})
	if err := NewTuicJob().replayTuicJournal(); err == nil || sharedTuicPoll.unlock == nil || !sharedTuicPoll.deleteUncertain {
		t.Fatalf("recovery released uncertain lock: %+v %v", sharedTuicPoll, err)
	}
	if err := db.Exec("DROP TRIGGER reject_recovery_tuic_marker").Error; err != nil {
		t.Fatal(err)
	}
	if err := service.FinishXrayPoll(batch.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := collectAndCommitTuic(&sharedTuicPoll, nil, &service.InboundService{}); err != nil || sharedTuicPoll.unlock != nil {
		t.Fatalf("recovery retry failed: %v", err)
	}
}

func TestTuicPeriodicRecoveryRequiresExclusiveMarkerAndMatchingReceipt(t *testing.T) {
	for _, mode := range []string{"foreign-marker", "missing-marker", "mismatched-receipt", "legacy-receipt"} {
		t.Run(mode, func(t *testing.T) {
			setupIntegrationDB(t)
			db := database.GetDB()
			row := xray.ClientTraffic{Email: "recovery-tuic@example.test", Enable: true}
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			batch := tuicTrafficBatch{ID: uuid.NewString(), PollMarker: true, Deltas: []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 25}}}
			if mode != "missing-marker" {
				if err := service.BeginXrayPoll(batch.ID); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "foreign-marker" {
				if err := service.BeginXrayPoll(uuid.NewString()); err != nil {
					t.Fatal(err)
				}
			}
			path, err := storeTuicBatch(batch)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "legacy-receipt" {
				if err := db.Create(&model.TuicTrafficReceipt{ID: batch.ID}).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				committed := append([]tuic.ClientTrafficDelta(nil), batch.Deltas...)
				if mode == "mismatched-receipt" {
					committed[0].Up = 26
				}
				if err := (&service.InboundService{}).AddTuicTrafficBatch(batch.ID, committed); err != nil {
					t.Fatal(err)
				}
			}
			if err := NewTuicJob().replayTuicJournal(); err == nil {
				t.Fatal("unsafe periodic recovery accepted")
			}
			if sharedTuicPoll.unlock != nil || sharedTuicPoll.batch != nil {
				t.Fatalf("invalid recovery kept process lock: %+v", sharedTuicPoll)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("journal evidence removed: %v", err)
			}
			if mode != "missing-marker" {
				if err := service.VerifyXrayPollMarker(batch.ID); err != nil {
					t.Fatalf("marker removed: %v", err)
				}
			}
		})
	}
}

func TestTuicPeriodicJournalAfterCrashRequiresReview(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	row := xray.ClientTraffic{Email: "crash-tuic@example.test", Enable: true}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_crash_tuic BEFORE UPDATE ON client_traffics BEGIN SELECT RAISE(ABORT, 'injected'); END").Error; err != nil {
		t.Fatal(err)
	}
	state := &tuicPollState{}
	if _, _, _, err := collectAndCommitTuic(state, func() []tuic.ClientTrafficDelta {
		return []tuic.ClientTrafficDelta{{Email: row.Email, TrafficID: row.Id, Up: 12}}
	}, &service.InboundService{}); err == nil {
		t.Fatal("expected injected failure")
	}
	id := state.batch.ID
	path := filepath.Join(tuicJournalDir(), id+".json")
	state.unlock() // Simulate process death while keeping marker and journal.
	state.unlock = nil
	if err := NewTuicJob().replayTuicJournal(); err == nil {
		t.Fatal("periodic journal automatically replayed without recovery coordination")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("periodic evidence lost: %v", err)
	}
	if err := service.VerifyXrayPollMarker(id); err != nil {
		t.Fatalf("periodic marker lost: %v", err)
	}
	if unlock, err := service.LockResellerAllocation(); err == nil {
		unlock()
		t.Fatal("orphan periodic marker allowed allocation")
	}
}
