package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Only an isolated PostgreSQL test database is suitable for this test.
func TestCounterIncidentAcrossPostgresProcesses(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{DisableForeignKeyConstraintWhenMigrating: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	mode := os.Getenv("PHOENIX_COUNTER_PG_HELPER")
	if mode != "" {
		email := os.Getenv("PHOENIX_COUNTER_PG_EMAIL")
		id, err := strconv.Atoi(os.Getenv("PHOENIX_COUNTER_PG_RESELLER"))
		if err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "write":
			if err := recordLocalXrayCounterIncident(db, &xray.UserTrafficCounterDecrease{Email: email, Direction: "uplink", Previous: 120, Observed: 2}); err != nil {
				t.Fatal(err)
			}
		case "check":
			if err := diskCounterIncidentError(); err != nil {
				t.Fatalf("reader directory is not isolated: %v", err)
			}
			if err := validateResellerNodeBaselines(db, id); err == nil {
				t.Fatal("shared database incident did not block reseller")
			}
			if err := checkLocalXrayCounterIncidents(db); err == nil {
				t.Fatal("shared database incident did not block local collection")
			}
		default:
			t.Fatalf("unknown helper %q", mode)
		}
		return
	}
	if err := db.AutoMigrate(&model.Reseller{}, &model.ResellerClient{}, &model.ClientRecord{}, &model.ClientInbound{}, &model.Inbound{}, &model.NodeClientTraffic{}, &model.ResellerNodeCounterIncident{}); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	reseller := model.Reseller{Username: "incident-pg-" + key, Password: "hash", Enable: true}
	if err := db.Create(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	email := key + "@incident.test"
	t.Cleanup(func() {
		_ = db.Where("email = ?", email).Delete(&model.ResellerNodeCounterIncident{}).Error
		_ = db.Where("reseller_id = ?", reseller.Id).Delete(&model.ResellerClient{}).Error
		_ = db.Delete(&reseller).Error
	})
	if err := db.Create(&model.ResellerClient{ResellerId: reseller.Id, Email: email}).Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, phase := range []string{"write", "check"} {
		dir := filepath.Join(root, phase)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCounterIncidentAcrossPostgresProcesses$")
		cmd.Env = append(os.Environ(), "PHOENIX_COUNTER_PG_HELPER="+phase, "PHOENIX_COUNTER_PG_EMAIL="+email, "PHOENIX_COUNTER_PG_RESELLER="+strconv.Itoa(reseller.Id), "XUI_DB_FOLDER="+dir)
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("helper %s: %v\n%s", phase, err, output)
		}
	}
}
