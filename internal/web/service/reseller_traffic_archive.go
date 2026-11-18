package service

import (
	"errors"
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

// closeResellerTrafficWindowTx records the current window before counters are
// cleared. Call inside the traffic writer's reset transaction. Historical
// unmapped archives are rolled forward too; only the current owner receives a
// new snapshot. Repeating after a successful reset records an empty window.
func closeResellerTrafficWindowTx(tx *gorm.DB, email string) error {
	key := transferEmailKey(email)
	var mappings []model.ResellerClient
	if err := tx.Where("LOWER(TRIM(email)) = ?", key).Find(&mappings).Error; err != nil {
		return err
	}
	var clients []model.ClientRecord
	if err := tx.Where("LOWER(TRIM(email)) = ?", key).Find(&clients).Error; err != nil {
		return err
	}
	if len(clients) != 1 {
		if len(mappings) != 0 {
			return errors.New("cannot renew reseller client without a unique client record")
		}
		return nil
	}
	client := &clients[0]
	if err := archiveResellerClientUsageTx(tx, client, 0); err != nil {
		return err
	}
	var archives []model.ResellerTrafficArchive
	if err := tx.Where("client_record_id = ?", client.Id).Find(&archives).Error; err != nil {
		return err
	}
	for _, entry := range archives {
		prior := saturatingAddBytes(entry.PriorConsumedBytes, entry.ConsumedBytes)
		if err := tx.Model(&model.ResellerTrafficArchive{}).Where("id = ?", entry.Id).Updates(map[string]any{
			"prior_consumed_bytes": prior,
			"consumed_bytes":       0,
			"updated_at":           time.Now().UnixMilli(),
		}).Error; err != nil {
			return err
		}
	}
	return nil
}

// closeResellerTrafficWindowsTx deduplicates identities so bulk reset never
// closes the same pre-reset counter twice.
func closeResellerTrafficWindowsTx(tx *gorm.DB, emails []string) error {
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
		if err := closeResellerTrafficWindowTx(tx, key); err != nil {
			return err
		}
	}
	return nil
}
