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
