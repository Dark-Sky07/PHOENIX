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

// ResellerBatchIntentEntry contains only the identity and allocation metadata
// needed to investigate an interrupted batch. Subscription IDs are sensitive;
// the table intentionally excludes passwords and protocol credentials.
type ResellerBatchIntentEntry struct {
	Email          string                     `json:"email"`
	SubID          string                     `json:"subId,omitempty"`
	RequestedBytes int64                      `json:"requestedBytes"`
	InboundIDs     []int                      `json:"inboundIds,omitempty"`
	InboundRefs    []ClientTransferInboundRef `json:"inboundRefs,omitempty"`
}

// BeginResellerBatchOperation must be called under the allocation lock before
// the first mutation. A batch may have partially succeeded, so failures retain
// the marker for manual reconciliation.
func BeginResellerBatchOperation(id int, operation string, entries []ResellerBatchIntentEntry) (*model.ResellerCreationIntent, error) {
	if id <= 0 || (operation != "bulk_create" && operation != "import") || len(entries) == 0 {
		return nil, errors.New("invalid reseller batch intent")
	}
	if err := CheckPendingResellerCreations(id); err != nil {
		return nil, err
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	intent := &model.ResellerCreationIntent{Id: uuid.NewString(), ResellerId: id, Email: "[batch]", SubID: uuid.NewString(), InboundIDs: "[]", Operation: operation, Details: string(data)}
	if err := database.GetDB().Create(intent).Error; err != nil {
		return nil, err
	}
	return intent, nil
}

// ResellerCreationIssue is the non-secret operational summary for an admin.
// Raw subscription IDs and batch manifests remain in the protected database.
type ResellerCreationIssue struct {
	ID        string `json:"id"`
	Operation string `json:"operation"`
	Email     string `json:"email"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *ResellerService) CreationIssues(id int) ([]ResellerCreationIssue, error) {
	if id <= 0 {
		return nil, errors.New("invalid reseller id")
	}
	rows := make([]ResellerCreationIssue, 0)
	err := database.GetDB().Model(&model.ResellerCreationIntent{}).Select("id", "operation", "email", "created_at").Where("reseller_id = ?", id).Order("created_at DESC, id DESC").Limit(100).Scan(&rows).Error
	return rows, err
}
