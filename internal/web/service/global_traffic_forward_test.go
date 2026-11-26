package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

func TestNodeGlobalTrafficForwardingFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		setup       func(*testing.T)
		wantFailure bool
	}{
		{"valid", func(t *testing.T) {}, false},
		{"overlay table unavailable", func(t *testing.T) {
			if err := databaseForForwardTest(t).Migrator().DropTable(&model.ClientGlobalTraffic{}); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"negative local", func(t *testing.T) {
			if err := databaseForForwardTest(t).Model(&xray.ClientTraffic{}).Where("email = ?", "forward@example.test").Update("up", -1).Error; err != nil {
				t.Fatal(err)
			}
		}, true},
		{"negative global", func(t *testing.T) {
			if err := databaseForForwardTest(t).Create(&model.ClientGlobalTraffic{MasterGuid: "upstream", Email: "forward@example.test", Up: -1}).Error; err != nil {
				t.Fatal(err)
			}
		}, true},
		{"ambiguous local", func(t *testing.T) {
			if err := databaseForForwardTest(t).Create(&xray.ClientTraffic{Email: " FORWARD@EXAMPLE.TEST ", Enable: true}).Error; err != nil {
				t.Fatal(err)
			}
		}, true},
		{"ambiguous global", func(t *testing.T) {
			db := databaseForForwardTest(t)
			for _, email := range []string{"forward@example.test", " FORWARD@EXAMPLE.TEST "} {
				if err := db.Create(&model.ClientGlobalTraffic{MasterGuid: "upstream", Email: email, Up: 1}).Error; err != nil {
					t.Fatal(err)
				}
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := initTrafficTestDB(t)
			if err := db.Create(&xray.ClientTraffic{Email: "forward@example.test", Up: 20, Enable: true}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Create(&model.NodeClientTraffic{NodeId: 1, Email: "forward@example.test"}).Error; err != nil {
				t.Fatal(err)
			}
			tc.setup(t)
			rows, err := (&InboundService{}).GetNodeClientTraffics(1)
			if tc.wantFailure && (err == nil || rows != nil) {
				t.Fatalf("unsafe snapshot returned: %+v %v", rows, err)
			}
			if !tc.wantFailure && (err != nil || len(rows) != 1 || rows[0].Up != 20) {
				t.Fatalf("valid snapshot failed: %+v %v", rows, err)
			}
		})
	}
}

func databaseForForwardTest(t *testing.T) *gorm.DB { t.Helper(); return database.GetDB() }
