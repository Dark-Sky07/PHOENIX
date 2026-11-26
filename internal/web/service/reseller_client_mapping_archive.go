package service

import (
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
)

// archiveResellerClientMappingsTx preserves ownership rows before a client
// deletion removes them. It runs inside the same transaction as the deletion,
// so a failed delete cannot leave an archive without the corresponding cleanup.
func archiveResellerClientMappingsTx(tx *gorm.DB, emails []string, reason string) error {
	if tx == nil || len(emails) == 0 {
		return nil
	}
	keys := make([]string, 0, len(emails))
	seen := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		key := transferEmailKey(email)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil
	}
	var mappings []model.ResellerClient
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var rows []model.ResellerClient
		if err := tx.Where("LOWER(TRIM(email)) IN ?", batch).Find(&rows).Error; err != nil {
			return err
		}
		mappings = append(mappings, rows...)
	}
	if len(mappings) == 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	archives := make([]model.ResellerClientMappingArchive, 0, len(mappings))
	for _, mapping := range mappings {
		archives = append(archives, model.ResellerClientMappingArchive{
			ResellerID: mapping.ResellerId,
			Email:      mapping.Email,
			CreatedAt:  mapping.CreatedAt,
			ArchivedAt: now,
			Reason:     reason,
		})
	}
	return tx.Create(&archives).Error
}
