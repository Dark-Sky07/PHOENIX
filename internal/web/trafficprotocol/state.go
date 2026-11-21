package trafficprotocol

import (
	"errors"
	"math"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
)

// ReserveSequence commits a sequence reservation before returning it to the
// caller. State must have been registered explicitly; this never creates or
// rotates identities. An uncertain commit must not be retried with the same
// number: reserve again. Gaps are safe. db must not already be a transaction.
func ReserveSequence(db *gorm.DB, id string) (Envelope, error) {
	if db == nil || id == "" {
		return Envelope{}, errors.New("missing traffic sender state")
	}
	var result Envelope
	err := db.Transaction(func(tx *gorm.DB) error {
		var state model.TrafficSnapshotSender
		if err := tx.First(&state, "id = ?", id).Error; err != nil {
			return err
		}
		if state.Sender == "" || state.Generation == "" || state.LastSequence < 0 || state.LastSequence == math.MaxInt64 {
			return errors.New("invalid or exhausted traffic sender state")
		}
		next := state.LastSequence + 1
		updated := tx.Model(&model.TrafficSnapshotSender{}).Where("id = ? AND sender = ? AND generation = ? AND last_sequence = ?", id, state.Sender, state.Generation, state.LastSequence).Update("last_sequence", next)
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("concurrent traffic sequence reservation")
		}
		result = Envelope{Version: Version, Sender: state.Sender, Generation: state.Generation, Sequence: uint64(next)}
		return nil
	})
	if err != nil {
		return Envelope{}, err
	}
	return result, nil
}

// ApplyVerified atomically authenticates an envelope against the receiver's
// registered identity, advances its sequence, and applies its snapshot using
// only tx. The callback must not perform external side effects; a rollback or
// concurrent CAS failure cannot undo those. The caller must provide the secret
// associated with id from trusted provisioning, not from the request. db must
// not already be a transaction. Neither DB rollback nor key compromise is
// addressed here. This is not wired to production endpoints.
func ApplyVerified(db *gorm.DB, id string, key []byte, envelope Envelope, apply func(*gorm.DB, []byte) error) error {
	if db == nil || id == "" || apply == nil {
		return errors.New("missing traffic receiver state or apply function")
	}
	if envelope.Sequence > math.MaxInt64 {
		return errors.New("traffic sequence exceeds persistent range")
	}
	if err := validate(envelope, key); err != nil {
		return err
	}
	if len(envelope.MAC) != 32 {
		return errors.New("invalid traffic snapshot authentication")
	}
	// Keep the bytes authenticated by Verify identical to what is applied.
	envelope.Snapshot = append([]byte(nil), envelope.Snapshot...)
	envelope.MAC = append([]byte(nil), envelope.MAC...)
	return db.Transaction(func(tx *gorm.DB) error {
		var state model.TrafficSnapshotReceiver
		if err := tx.First(&state, "id = ?", id).Error; err != nil {
			return err
		}
		if state.LastSequence < 0 {
			return errors.New("invalid stored traffic sequence")
		}
		if err := Verify(envelope, key, Expected{Sender: state.Sender, Generation: state.Generation, LastSequence: uint64(state.LastSequence)}); err != nil {
			return err
		}
		updated := tx.Model(&model.TrafficSnapshotReceiver{}).Where("id = ? AND sender = ? AND generation = ? AND last_sequence = ?", id, state.Sender, state.Generation, state.LastSequence).Update("last_sequence", int64(envelope.Sequence))
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errors.New("concurrent traffic snapshot update")
		}
		return apply(tx, envelope.Snapshot)
	})
}
