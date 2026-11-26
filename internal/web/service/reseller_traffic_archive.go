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
	if err := rejectUnprovenDaemonRemoval(tx, []string{client.Email}, nil); err != nil {
		return err
	}
	if err := checkPPPAndL2TPAccountingIncidents(tx); err != nil {
		return err
	}
	if err := rejectClientNodeIncidents(tx, []string{client.Email}); err != nil {
		return err
	}
	var corruptArchives int64
	if err := tx.Model(&model.ResellerTrafficArchive{}).
		Where("client_record_id = ? AND (consumed_bytes < 0 OR prior_consumed_bytes < 0)", client.Id).
		Count(&corruptArchives).Error; err != nil {
		return err
	}
	if corruptArchives > 0 {
		return errors.New("negative reseller traffic archive requires reconciliation")
	}
	var rows []xray.ClientTraffic
	if err := tx.Where("LOWER(TRIM(email)) = ?", transferEmailKey(client.Email)).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) > 1 {
		return errors.New("ambiguous client traffic counters require reconciliation")
	}
	var matchingClients int64
	if err := tx.Model(&model.ClientRecord{}).Where("LOWER(TRIM(email)) = ?", transferEmailKey(client.Email)).Count(&matchingClients).Error; err != nil {
		return err
	}
	if matchingClients != 1 {
		return errors.New("ambiguous reseller client requires reconciliation")
	}
	var globals []model.ClientGlobalTraffic
	if err := tx.Where("LOWER(TRIM(email)) = ?", transferEmailKey(client.Email)).Find(&globals).Error; err != nil {
		return err
	}
	var globalUp, globalDown int64
	for _, row := range globals {
		if row.Up < 0 || row.Down < 0 {
			return errors.New("negative reseller global traffic requires reconciliation")
		}
		globalUp = max(globalUp, row.Up)
		globalDown = max(globalDown, row.Down)
	}
	var used int64
	for _, row := range rows {
		if row.Up < 0 || row.Down < 0 {
			return errors.New("negative reseller client traffic requires reconciliation")
		}
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
// rejectNegativeTrafficEvidence protects local and upstream records even for
// clients without a reseller mapping or canonical record. Call before any
// runtime enablement and repeat inside the counter-reset transaction.
func rejectNegativeTrafficEvidence(db *gorm.DB, emails []string) error {
	keys := make([]string, 0, len(emails))
	for _, email := range emails {
		if key := transferEmailKey(email); key != "" {
			keys = append(keys, key)
		}
	}
	for _, batch := range chunkStrings(keys, sqliteMaxVars) {
		for _, table := range []any{&xray.ClientTraffic{}, &model.ClientGlobalTraffic{}} {
			var count int64
			if err := db.Model(table).Where("LOWER(TRIM(email)) IN ? AND (up < 0 OR down < 0)", batch).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return errors.New("negative client traffic requires manual reconciliation before reset")
			}
		}
	}
	return nil
}

func closeResellerTrafficWindowTx(tx *gorm.DB, email string) error {
	if err := rejectNegativeTrafficEvidence(tx, []string{email}); err != nil {
		return err
	}
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
		if entry.PriorConsumedBytes < 0 || entry.ConsumedBytes < 0 {
			return errors.New("negative reseller traffic archive requires reconciliation")
		}
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

// rewriteResellerTrafficCountersTx preserves a current owner's usage floor when
// an administrator reduces counters. Only the lost difference moves to prior
// usage, so increasing counters is not double charged. Unassigned owners keep
// their previously recorded snapshots rather than acquiring later usage.
func rewriteResellerTrafficCountersTx(tx *gorm.DB, email string, upload, download int64) error {
	if err := rejectNegativeTrafficEvidence(tx, []string{email}); err != nil {
		return err
	}
	key := transferEmailKey(email)
	var rows []xray.ClientTraffic
	if err := tx.Where("LOWER(TRIM(email)) = ?", key).Find(&rows).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if len(rows) != 1 {
		return errors.New("cannot rewrite ambiguous traffic counters")
	}
	var mappings []model.ResellerClient
	if err := tx.Where("LOWER(TRIM(email)) = ?", key).Find(&mappings).Error; err != nil {
		return err
	}
	var clients []model.ClientRecord
	if err := tx.Where("LOWER(TRIM(email)) = ?", key).Find(&clients).Error; err != nil {
		return err
	}
	if len(clients) != 1 && len(mappings) > 0 {
		return errors.New("cannot rewrite reseller traffic without a unique client record")
	}
	if len(clients) == 1 {
		client := &clients[0]
		if err := archiveResellerClientUsageTx(tx, client, 0); err != nil {
			return err
		}
		var globals []model.ClientGlobalTraffic
		if err := tx.Where("LOWER(TRIM(email)) = ?", key).Find(&globals).Error; err != nil {
			return err
		}
		globalUp, globalDown := int64(0), int64(0)
		for _, row := range globals {
			globalUp = max(globalUp, row.Up)
			globalDown = max(globalDown, row.Down)
		}
		newUsed := saturatingAddBytes(max(upload, globalUp), max(download, globalDown))
		mapped := make(map[int]struct{}, len(mappings))
		for _, mapping := range mappings {
			mapped[mapping.ResellerId] = struct{}{}
		}
		var archives []model.ResellerTrafficArchive
		if err := tx.Where("client_record_id = ?", client.Id).Find(&archives).Error; err != nil {
			return err
		}
		for _, entry := range archives {
			closed := entry.ConsumedBytes
			if _, active := mapped[entry.ResellerId]; active {
				if closed > newUsed {
					closed -= newUsed
				} else {
					closed = 0
				}
			}
			if err := tx.Model(&model.ResellerTrafficArchive{}).Where("id = ?", entry.Id).Updates(map[string]any{
				"prior_consumed_bytes": saturatingAddBytes(entry.PriorConsumedBytes, closed),
				"consumed_bytes":       0,
				"updated_at":           time.Now().UnixMilli(),
			}).Error; err != nil {
				return err
			}
		}
	}
	return tx.Model(&xray.ClientTraffic{}).Where("id = ?", rows[0].Id).Updates(map[string]any{"up": upload, "down": download}).Error
}
