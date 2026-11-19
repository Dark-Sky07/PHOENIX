package service

import (
	"fmt"
	"sort"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
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

func verifyXrayTrafficIdentities(tx *gorm.DB, traffic []*xray.ClientTraffic, expected []XrayTrafficIdentity) error {
	actual, err := captureXrayTrafficIdentities(tx, traffic)
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("Xray traffic identity set changed; manual reconciliation required")
	}
	for i := range actual {
		if actual[i].Email != expected[i].Email || !equalIDs(actual[i].ClientIDs, expected[i].ClientIDs) || !equalIDs(actual[i].TrafficIDs, expected[i].TrafficIDs) || !equalIDs(actual[i].ResellerIDs, expected[i].ResellerIDs) {
			return fmt.Errorf("Xray traffic identity or ownership changed for %q; manual reconciliation required", actual[i].Email)
		}
	}
	return nil
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
