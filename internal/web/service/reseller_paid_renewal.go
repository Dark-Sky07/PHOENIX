package service

import (
	"errors"
	"math"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/gorm"
)

// validatePaidClientRenewal checks allocation before a payment webhook changes
// a mapped client. The allocation lock must already be held by the caller.
// A reset moves usage into the archive, so the new window must reserve both
// the previously used bytes and the new quota.
func validatePaidClientRenewal(client *model.ClientRecord, newQuota int64, reset bool) error {
	if client == nil || newQuota < 0 {
		return errors.New("invalid client renewal quota")
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		var mappings []model.ResellerClient
		if err := tx.Where("LOWER(TRIM(email)) = ?", transferEmailKey(client.Email)).Find(&mappings).Error; err != nil {
			return err
		}
		tracker := renewalQuotaTracker{tx: tx, accounts: map[int]*renewalQuotaSnapshot{}}
		seen := map[int]bool{}
		for _, m := range mappings {
			if seen[m.ResellerId] {
				continue
			}
			seen[m.ResellerId] = true
			state, err := tracker.load(m.ResellerId)
			if err != nil {
				return err
			}
			oldQuota, ok := state.currentQuota[client.Id]
			if !ok {
				return errors.New("reseller renewal ownership changed")
			}
			if state.disabled {
				return errors.New("reseller account is disabled or expired")
			}
			if state.limit <= 0 {
				continue
			}
			if state.unlimitedClient {
				return errors.New("limited reseller has an unlimited client; administrator reconciliation required")
			}
			if newQuota == 0 {
				return errors.New("unlimited client traffic is not allowed for a limited reseller")
			}
			used := state.currentUsage[client.Id]
			oldEffective := max(oldQuota, used)
			var newEffective int64
			if reset {
				if used > math.MaxInt64-newQuota {
					return errors.New("renewal allocation exceeds supported range")
				}
				newEffective = used + newQuota
			} else {
				newEffective = max(used, newQuota)
			}
			delta := max(int64(0), newEffective-oldEffective)
			if state.used >= state.limit || state.allocated > state.limit || delta > state.limit-state.allocated {
				return errors.New("reseller traffic quota exceeded by paid client renewal")
			}
		}
		return nil
	})
}
