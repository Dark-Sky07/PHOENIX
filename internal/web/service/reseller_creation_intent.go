package service

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/google/uuid"
)

func CheckPendingResellerCreations(id int) error {
	var count int64
	if err := database.GetDB().Model(&model.ResellerCreationIntent{}).Where("reseller_id = ?", id).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return errors.New("reseller client creation needs manual reconciliation; contact the administrator")
	}
	return nil
}

// Call only under the allocation lock. The marker is written before any
// client or runtime mutation, and kept if compensation is uncertain.
func BeginResellerClientCreation(id int, payload *ClientCreatePayload) (*model.ResellerCreationIntent, error) {
	if payload == nil || id <= 0 || strings.TrimSpace(payload.Client.Email) == "" || strings.TrimSpace(payload.Client.SubID) == "" {
		return nil, errors.New("creation intent requires client identity")
	}
	if err := CheckPendingResellerCreations(id); err != nil {
		return nil, err
	}
	inboundIDs, err := json.Marshal(payload.InboundIds)
	if err != nil {
		return nil, err
	}
	intent := &model.ResellerCreationIntent{Id: uuid.NewString(), ResellerId: id, Email: strings.TrimSpace(payload.Client.Email), SubID: payload.Client.SubID, RequestedBytes: payload.Client.TotalGB, InboundIDs: string(inboundIDs)}
	if payload.SalePrice != nil {
		amount := payload.SalePrice.AmountMinor
		intent.AmountMinor = &amount
		intent.Currency = payload.SalePrice.Currency
	}
	if err := database.GetDB().Create(intent).Error; err != nil {
		return nil, err
	}
	return intent, nil
}

// Discard is only safe after the caller verified compensation succeeded.
func DiscardResellerClientCreation(intent *model.ResellerCreationIntent) error {
	if intent == nil {
		return nil
	}
	result := database.GetDB().Where("id = ? AND reseller_id = ?", intent.Id, intent.ResellerId).Delete(&model.ResellerCreationIntent{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return errors.New("creation intent missing; manual reconciliation required")
	}
	return nil
}

func ResellerCreationIntentExists(intent *model.ResellerCreationIntent) (bool, error) {
	var count int64
	err := database.GetDB().Model(&model.ResellerCreationIntent{}).Where("id = ? AND reseller_id = ?", intent.Id, intent.ResellerId).Count(&count).Error
	return count == 1, err
}
