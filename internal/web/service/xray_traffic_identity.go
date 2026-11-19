package service

import (
	"fmt"
	"sort"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// XrayTrafficIdentity binds a journaled delta to the database identities and
// ownership observed when the batch was prepared. It is not a distributed lock.
type XrayTrafficIdentity struct {
	Email       string `json:"email"`
	ClientIDs   []int  `json:"clientIds"`
	TrafficIDs  []int  `json:"trafficIds"`
	ResellerIDs []int  `json:"resellerIds"`
}

func (s *InboundService) CaptureXrayTrafficIdentities(traffic []*xray.ClientTraffic) ([]XrayTrafficIdentity, error) {
	return captureXrayTrafficIdentities(database.GetDB(), traffic)
}

func captureXrayTrafficIdentities(db *gorm.DB, traffic []*xray.ClientTraffic) ([]XrayTrafficIdentity, error) {
	set := map[string]struct{}{}
	for _, row := range traffic {
		if row != nil && (row.Up != 0 || row.Down != 0) {
			if key := transferEmailKey(row.Email); key != "" {
				set[key] = struct{}{}
			}
		}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	identities := make([]XrayTrafficIdentity, len(keys))
	byEmail := make(map[string]*XrayTrafficIdentity, len(keys))
	for i, key := range keys {
		identities[i] = XrayTrafficIdentity{Email: key}
		byEmail[key] = &identities[i]
	}
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var clients []model.ClientRecord
		if err := db.Select("id", "email").Where("LOWER(TRIM(email)) IN ?", batch).Find(&clients).Error; err != nil {
			return nil, err
		}
		for _, c := range clients {
			byEmail[transferEmailKey(c.Email)].ClientIDs = append(byEmail[transferEmailKey(c.Email)].ClientIDs, c.Id)
		}
		var counters []xray.ClientTraffic
		if err := db.Select("id", "email").Where("LOWER(TRIM(email)) IN ?", batch).Find(&counters).Error; err != nil {
			return nil, err
		}
		for _, c := range counters {
			byEmail[transferEmailKey(c.Email)].TrafficIDs = append(byEmail[transferEmailKey(c.Email)].TrafficIDs, c.Id)
		}
		var owners []model.ResellerClient
		if err := db.Select("email", "reseller_id").Where("LOWER(TRIM(email)) IN ?", batch).Find(&owners).Error; err != nil {
			return nil, err
		}
		for _, c := range owners {
			byEmail[transferEmailKey(c.Email)].ResellerIDs = append(byEmail[transferEmailKey(c.Email)].ResellerIDs, c.ResellerId)
		}
	}
	for i := range identities {
		sort.Ints(identities[i].ClientIDs)
		sort.Ints(identities[i].TrafficIDs)
		sort.Ints(identities[i].ResellerIDs)
	}
	return identities, nil
}

// reconcileXrayTrafficIdentities keeps ordinary live deltas attached to their
// observed identity. If a client was removed or transferred after the batch
// was captured, a unique archived owner can safely receive the late bytes.
// Ambiguous identities remain in the journal for manual reconciliation.
func reconcileXrayTrafficIdentities(tx *gorm.DB, traffic []*xray.ClientTraffic, expected []XrayTrafficIdentity) ([]*xray.ClientTraffic, error) {
	actual, err := captureXrayTrafficIdentities(tx, traffic)
	if err != nil {
		return nil, err
	}
	if len(actual) != len(expected) {
		return nil, fmt.Errorf("Xray traffic identity set changed; manual reconciliation required")
	}
	deltas := make(map[string]int64, len(expected))
	for _, row := range traffic {
		if row == nil {
			continue
		}
		if row.Up < 0 || row.Down < 0 {
			return nil, fmt.Errorf("negative Xray traffic delta")
		}
		key := transferEmailKey(row.Email)
		deltas[key] = saturatingAddBytes(deltas[key], saturatingAddBytes(row.Up, row.Down))
	}
	retired := make(map[string]struct{})
	for i := range actual {
		old, current := expected[i], actual[i]
		if old.Email != current.Email {
			return nil, fmt.Errorf("Xray traffic identity set changed; manual reconciliation required")
		}
		if equalIDs(current.ClientIDs, old.ClientIDs) && equalIDs(current.TrafficIDs, old.TrafficIDs) && equalIDs(current.ResellerIDs, old.ResellerIDs) {
			continue
		}
		if len(old.ClientIDs) != 1 || len(old.TrafficIDs) != 1 || len(old.ResellerIDs) != 1 {
			return nil, fmt.Errorf("Xray traffic identity or ownership changed for %q; manual reconciliation required", current.Email)
		}
		var archive model.ResellerTrafficArchive
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("reseller_id = ? AND client_record_id = ?", old.ResellerIDs[0], old.ClientIDs[0]).Take(&archive).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return nil, fmt.Errorf("Xray traffic archive for %q missing; manual reconciliation required", current.Email)
			}
			return nil, err
		}
		if transferEmailKey(archive.Email) != old.Email {
			return nil, fmt.Errorf("Xray traffic archive identity mismatch for %q; manual reconciliation required", current.Email)
		}
		if err := tx.Model(&model.ResellerTrafficArchive{}).Where("id = ?", archive.Id).Update("prior_consumed_bytes", saturatingAddBytes(archive.PriorConsumedBytes, deltas[current.Email])).Error; err != nil {
			return nil, err
		}
		retired[current.Email] = struct{}{}
	}
	if len(retired) == 0 {
		return traffic, nil
	}
	live := make([]*xray.ClientTraffic, 0, len(traffic))
	for _, row := range traffic {
		if row == nil {
			continue
		}
		if _, ok := retired[transferEmailKey(row.Email)]; !ok {
			live = append(live, row)
		}
	}
	return live, nil
}

func equalIDs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
