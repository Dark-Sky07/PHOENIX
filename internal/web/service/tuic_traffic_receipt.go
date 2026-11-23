package service

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// validateTuicJournalDestinations prevents canonicalizeClientTraffic from
// silently discarding a deleted traffic ID, falling back to a reused email,
// or charging several case variants. Row IDs preserve supported renames but
// do not prove a client's original generation or immutable reseller ownership.
func validateTuicJournalDestinations(tx *gorm.DB, traffic []*xray.ClientTraffic) error {
	for _, sample := range traffic {
		if sample == nil {
			return errors.New("nil TUIC journal sample")
		}
		if sample.Up < 0 || sample.Down < 0 {
			return errors.New("negative TUIC journal traffic")
		}
		if sample.Up == 0 && sample.Down == 0 {
			continue
		}
		if sample.TuicTrafficID <= 0 {
			return errors.New("TUIC journal sample lacks traffic identity; manual reconciliation required")
		}
		var row xray.ClientTraffic
		if err := tx.Select("id", "email").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", sample.TuicTrafficID).Take(&row).Error; err != nil {
			return fmt.Errorf("TUIC journal traffic identity %d unavailable: %w", sample.TuicTrafficID, err)
		}
		key := transferEmailKey(row.Email)
		if key == "" {
			return errors.New("TUIC journal traffic identity has empty email")
		}
		var matches []xray.ClientTraffic
		if err := tx.Select("id").Clauses(clause.Locking{Strength: "UPDATE"}).Where("LOWER(TRIM(email)) = ?", key).Order("id").Find(&matches).Error; err != nil {
			return err
		}
		if len(matches) != 1 || matches[0].Id != sample.TuicTrafficID {
			return fmt.Errorf("TUIC journal traffic identity %d has ambiguous destination; manual reconciliation required", sample.TuicTrafficID)
		}
	}
	return nil
}

// AddTuicTrafficBatch commits a journal batch and its receipt atomically. A
// retained journal can therefore be replayed after an ambiguous commit result.
func (s *InboundService) AddTuicTrafficBatch(id string, traffic []*xray.ClientTraffic) error {
	if id == "" {
		return errors.New("TUIC traffic batch has no ID")
	}
	return submitTrafficWrite(func() error {
		return database.GetDB().Transaction(func(tx *gorm.DB) error {
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.TuicTrafficReceipt{ID: id})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				return nil
			}
			if err := validateTuicJournalDestinations(tx, traffic); err != nil {
				return err
			}
			return s.addClientTraffic(tx, traffic)
		})
	})
}
