package service

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

// validateTuicJournalDestinations prevents canonicalizeClientTraffic from
// silently discarding a deleted traffic ID, falling back to a reused email,
// or charging several case variants. Row IDs preserve supported renames but
// do not prove a client's original generation or immutable reseller ownership.
func validateTuicJournalDestinations(tx *gorm.DB, traffic []*xray.ClientTraffic) error {
	if len(traffic) == 0 {
		return nil
	}
	// SQL LOWER/TRIM does not implement the same Unicode case folding and
	// whitespace handling as transferEmailKey. Scan all existing rows so that
	// an otherwise hidden duplicate cannot pass the destination check. This
	// is a point-in-time check, not a lock against concurrent insertions.
	var allRows []xray.ClientTraffic
	if err := tx.Select("id", "email").Find(&allRows).Error; err != nil {
		return err
	}
	byKey := make(map[string][]int, len(allRows))
	for _, row := range allRows {
		key := transferEmailKey(row.Email)
		byKey[key] = append(byKey[key], row.Id)
	}
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
		if ids := byKey[key]; len(ids) != 1 || ids[0] != sample.TuicTrafficID {
			return fmt.Errorf("TUIC journal traffic identity %d has ambiguous normalized destination; manual reconciliation required", sample.TuicTrafficID)
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

func tuicTrafficPayloadHash(id string, deltas []tuic.ClientTrafficDelta) (string, error) {
	if id == "" {
		return "", errors.New("TUIC traffic batch has no ID")
	}
	payload, err := json.Marshal(struct {
		ID     string                    `json:"id"`
		Deltas []tuic.ClientTrafficDelta `json:"deltas"`
	}{id, deltas})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("v1:%x", sha256.Sum256(payload)), nil
}

// VerifyCommittedTuicTrafficBatch only verifies an existing receipt; it never
// creates one or applies uncommitted deltas. Call under the allocation recovery
// lock, with the original marker still present.
func VerifyCommittedTuicTrafficBatch(id string, deltas []tuic.ClientTrafficDelta) error {
	hash, err := tuicTrafficPayloadHash(id, deltas)
	if err != nil {
		return err
	}
	db := database.GetDB()
	if db == nil {
		return errors.New("database unavailable while checking TUIC receipt")
	}
	var receipt model.TuicTrafficReceipt
	if err := db.Where("id = ?", id).Take(&receipt).Error; err != nil {
		return fmt.Errorf("TUIC committed receipt unavailable; manual reconciliation required: %w", err)
	}
	if receipt.PayloadHash == "" || receipt.PayloadHash != hash {
		return errors.New("TUIC receipt lacks matching payload evidence; manual reconciliation required")
	}
	return nil
}

// AddTuicTrafficBatch commits a journal batch and its receipt atomically. A
// retained journal can therefore be replayed after an ambiguous commit result.
func (s *InboundService) AddTuicTrafficBatch(id string, deltas []tuic.ClientTrafficDelta) error {
	hash, err := tuicTrafficPayloadHash(id, deltas)
	if err != nil {
		return err
	}
	traffic := make([]*xray.ClientTraffic, 0, len(deltas))
	for _, delta := range deltas {
		traffic = append(traffic, &xray.ClientTraffic{Email: delta.Email, TuicTrafficID: delta.TrafficID, TuicUUID: delta.UUID, TuicInboundId: delta.InboundID, Up: delta.Up, Down: delta.Down})
	}
	return submitTrafficWrite(func() error {
		return database.GetDB().Transaction(func(tx *gorm.DB) error {
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.TuicTrafficReceipt{ID: id, PayloadHash: hash})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 0 {
				var receipt model.TuicTrafficReceipt
				if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).Take(&receipt).Error; err != nil {
					return err
				}
				if receipt.PayloadHash == "" || receipt.PayloadHash != hash {
					return errors.New("TUIC receipt lacks matching payload evidence; manual reconciliation required")
				}
				return nil
			}
			if err := validateTuicJournalDestinations(tx, traffic); err != nil {
				return err
			}
			return s.addClientTraffic(tx, traffic)
		})
	})
}
