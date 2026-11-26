package service

import (
	"errors"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm/clause"
)

// BeginXrayPoll must run under the allocation lock before the runtime poll.
func BeginXrayPoll(id string) error {
	if id == "" || database.GetDB() == nil {
		return errors.New("invalid Xray poll marker")
	}
	return database.GetDB().Create(&model.XrayTrafficInFlight{ID: id}).Error
}

// FinishXrayPoll is idempotent for old journals created before poll markers.
// Call only after successful journal removal, or a poll known not to advance
// its baseline. Never infer traffic from an orphaned marker.
func FinishXrayPoll(id string) error {
	if id == "" || database.GetDB() == nil {
		return errors.New("invalid Xray poll marker")
	}
	return database.GetDB().Where("id = ?", id).Delete(&model.XrayTrafficInFlight{}).Error
}

// RecoverCommittedXrayPollMarkers clears only markers whose receipt proves
// that the batch transaction committed. Call under the allocation recovery
// lock. Receipts are deliberately retained: another host may still hold a
// journal and must be able to replay it without charging the batch twice.
// Markers without versioned receipts remain for journal recovery or manual
// review; legacy receipts cannot bind their marker to a particular payload.
func RecoverCommittedXrayPollMarkers() error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database unavailable while recovering Xray poll markers")
	}
	receipts := db.Model(&model.XrayTrafficReceipt{}).Select("id").Where("payload_hash LIKE ? AND LENGTH(payload_hash) = ?", "v1:%", 67)
	return db.Where("id IN (?)", receipts).Delete(&model.XrayTrafficInFlight{}).Error
}

// VerifyXrayPollMarker checks that a retry still has its original durable
// fence. Call under the allocation lock; a missing marker needs investigation.
func VerifyXrayPollMarker(id string) error {
	if id == "" || database.GetDB() == nil {
		return errors.New("invalid poll marker")
	}
	var count int64
	if err := database.GetDB().Model(&model.XrayTrafficInFlight{}).Where("id = ?", id).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return errors.New("poll marker disappeared; manual reconciliation required")
	}
	return nil
}

// VerifyExclusiveXrayPollMarker additionally rejects a foreign unresolved poll.
// Call under the allocation lock before re-polling after a temporary wait or
// retrying a PPP poll; a foreign unresolved marker must be recovered first.
func VerifyExclusiveXrayPollMarker(id string) error {
	if err := VerifyXrayPollMarker(id); err != nil {
		return err
	}
	var count int64
	if err := database.GetDB().Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return errors.New("another poll marker requires recovery before retry")
	}
	return nil
}

// EnsureXrayPollMarker re-establishes a fence after an uncertain delete. It
// must be called while continuously holding the allocation lock since deletion.
func EnsureXrayPollMarker(id string) error {
	if id == "" || database.GetDB() == nil {
		return errors.New("invalid poll marker")
	}
	return database.GetDB().Clauses(clause.OnConflict{DoNothing: true}).Create(&model.XrayTrafficInFlight{ID: id}).Error
}
