package service

import (
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// archiveResellerClientUsage snapshots persisted counters before client or
// mapping removal. Retrying a partial deletion never lowers the snapshot.
// This serializes with the traffic writer; it does not itself poll runtimes.
func archiveResellerClientUsage(client *model.ClientRecord) error {
	if client == nil || client.Id <= 0 || strings.TrimSpace(client.Email) == "" {
		return nil
	}
	return submitTrafficWrite(func() error {
		return database.GetDB().Transaction(func(tx *gorm.DB) error {
			return archiveResellerClientUsageTx(tx, client, 0)
		})
	})
}

// archiveResellerClientUsageTx stores a snapshot in the caller's transaction.
// resellerID zero snapshots all current mappings, otherwise only that owner.
func archiveResellerClientUsageTx(tx *gorm.DB, client *model.ClientRecord, resellerID int) error {
	if client == nil || client.Id <= 0 || strings.TrimSpace(client.Email) == "" {
		return nil
	}
	mappingQuery := tx.Where("LOWER(TRIM(email)) = ?", transferEmailKey(client.Email))
	if resellerID > 0 {
		mappingQuery = mappingQuery.Where("reseller_id = ?", resellerID)
	}
	var mappings []model.ResellerClient
	if err := mappingQuery.Find(&mappings).Error; err != nil {
		return err
	}
	if len(mappings) == 0 {
		return nil
	}
	var rows []xray.ClientTraffic
	if err := tx.Where("LOWER(TRIM(email)) = ?", transferEmailKey(client.Email)).Find(&rows).Error; err != nil {
		return err
	}
	var globals []model.ClientGlobalTraffic
	if err := tx.Where("LOWER(TRIM(email)) = ?", transferEmailKey(client.Email)).Find(&globals).Error; err != nil {
		return err
	}
	var globalUp, globalDown int64
	for _, row := range globals {
		globalUp = max(globalUp, row.Up)
		globalDown = max(globalDown, row.Down)
	}
	var used int64
	for _, row := range rows {
		used = saturatingAddBytes(used, max(row.Up, globalUp))
		used = saturatingAddBytes(used, max(row.Down, globalDown))
	}
	if len(rows) == 0 {
		used = saturatingAddBytes(globalUp, globalDown)
	}
	for _, mapping := range mappings {
		archive := model.ResellerTrafficArchive{ResellerId: mapping.ResellerId, ClientRecordId: client.Id, Email: client.Email, ConsumedBytes: used}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "reseller_id"}, {Name: "client_record_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"consumed_bytes": gorm.Expr("CASE WHEN consumed_bytes < ? THEN ? ELSE consumed_bytes END", used, used),
				"updated_at":     time.Now().UnixMilli(),
			}),
		}).Create(&archive).Error; err != nil {
			return err
		}
	}
	return nil
}
