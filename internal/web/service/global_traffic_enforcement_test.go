package service

import (
	"math"
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Run the exact predicate used in both selection and update, against SQLite
// and PostgreSQL. Maxima can come from different masters or local counters.
func testEffectiveTrafficPredicate(t *testing.T, db *gorm.DB) {
	t.Helper()
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	prefix := uuid.NewString()
	cases := []struct {
		name            string
		up, down, total int64
		masters         [][2]int64
		want            bool
	}{
		{"local_and_global", 600, 10, 1000, [][2]int64{{100, 400}}, true},
		{"different_masters", 10, 10, 1000, [][2]int64{{600, 100}, {100, 400}}, true},
		{"below_limit", 10, 10, 1001, [][2]int64{{600, 100}, {100, 400}}, false},
		{"unlimited", 600, 600, 0, [][2]int64{{900, 900}}, false},
		{"maxint_exact", math.MaxInt64 - 1, 1, math.MaxInt64, nil, true},
		{"maxint_under", math.MaxInt64 - 1, 0, math.MaxInt64, nil, false},
		{"maxint_over", math.MaxInt64, math.MaxInt64, math.MaxInt64, nil, true},
	}
	for _, c := range cases {
		email := prefix + "-" + c.name
		row := xray.ClientTraffic{Email: email, Enable: true, Up: c.up, Down: c.down, Total: c.total}
		if err := tx.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
		for i, v := range c.masters {
			g := model.ClientGlobalTraffic{Email: email, MasterGuid: prefix + string(rune('a'+i)), Up: v[0], Down: v[1]}
			if err := tx.Create(&g).Error; err != nil {
				t.Fatal(err)
			}
		}
		var count int64
		if err := tx.Model(&xray.ClientTraffic{}).Where("id = ?", row.Id).Where(depletedClientsCond, int64(1)).Count(&count).Error; err != nil {
			t.Fatalf("%s select: %v", c.name, err)
		}
		if (count == 1) != c.want {
			t.Errorf("%s: selected %d, want %v", c.name, count, c.want)
		}
		res := tx.Model(&xray.ClientTraffic{}).Where("id = ?", row.Id).Where(depletedClientsCond, int64(1)).Update("enable", false)
		if res.Error != nil || (res.RowsAffected == 1) != c.want {
			t.Errorf("%s update: count %d error %v", c.name, res.RowsAffected, res.Error)
		}
	}
}

func TestEffectiveGlobalTrafficEnforcementSQLite(t *testing.T) {
	db := initTrafficTestDB(t)
	testEffectiveTrafficPredicate(t, db)
	seedClientRow(t, "mixed-global", 1, 600, 10, 1000)
	if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "master", Email: "mixed-global", Up: 100, Down: 400}).Error; err != nil {
		t.Fatal(err)
	}
	if _, count, _, err := (&InboundService{}).disableInvalidClients(db); err != nil || count != 1 {
		t.Fatalf("disable: count=%d err=%v", count, err)
	}
	if readTraffic(t, db, "mixed-global").Enable {
		t.Fatal("effective global usage did not disable client")
	}
}

func TestEffectiveGlobalTrafficEnforcementPostgres(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.AutoMigrate(&xray.ClientTraffic{}, &model.ClientGlobalTraffic{}); err != nil {
		t.Fatal(err)
	}
	testEffectiveTrafficPredicate(t, db)
}
