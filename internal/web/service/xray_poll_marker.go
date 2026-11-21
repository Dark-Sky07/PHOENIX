package service

import (
	"errors"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
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
// Markers without receipts remain for journal recovery or manual review.
func RecoverCommittedXrayPollMarkers() error {
	db := database.GetDB()
	if db == nil {
		return errors.New("database unavailable while recovering Xray poll markers")
	}
	receipts := db.Model(&model.XrayTrafficReceipt{}).Select("id")
	return db.Where("id IN (?)", receipts).Delete(&model.XrayTrafficInFlight{}).Error
}
