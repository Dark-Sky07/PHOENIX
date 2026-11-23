package service

import (
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

// daemonSQLTrafficIDs identifies destinations that both SQL accounting and Go
// normalization resolve to the same single row. It is a point-in-time check,
// not a predicate lock or proof of historical accounting completeness.
func daemonSQLTrafficIDs(db *gorm.DB, emails []string) (map[string]int, error) {
	keys := make([]string, 0, len(emails))
	wanted := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		key := transferEmailKey(email)
		if key == "" {
			continue
		}
		if _, ok := wanted[key]; !ok {
			wanted[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	type result struct {
		ID     int
		Email  string
		SQLKey string `gorm:"column:sql_key"`
	}
	ids := make(map[string]int, len(keys))
	counts := make(map[string]int, len(keys))
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var rows []result
		if err := db.Model(&xray.ClientTraffic{}).Select("id, email, LOWER(TRIM(email)) AS sql_key").Where("LOWER(TRIM(email)) IN ?", batch).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, row := range rows {
			if _, ok := wanted[row.SQLKey]; !ok {
				continue
			}
			counts[row.SQLKey]++
			if row.SQLKey == transferEmailKey(row.Email) {
				ids[row.SQLKey] = row.ID
			} else {
				ids[row.SQLKey] = 0
			}
		}
	}
	for key, count := range counts {
		if count != 1 {
			ids[key] = 0
		}
	}
	return ids, nil
}
