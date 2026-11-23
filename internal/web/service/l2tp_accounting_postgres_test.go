package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Uses a dedicated PostgreSQL database and independently configured processes.
func TestL2TPIncidentAcrossPostgresProcesses(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	mode := os.Getenv("PHOENIX_L2TP_PG_HELPER")
	if mode != "" {
		t.Setenv("XUI_DB_TYPE", "postgres")
		t.Setenv("XUI_DB_DSN", dsn)
		if err := database.InitDB(""); err != nil {
			t.Fatal(err)
		}
		defer database.CloseDB()
		switch mode {
		case "write":
			if err := os.MkdirAll(filepath.Join(os.Getenv("XUI_BIN_FOLDER"), "l2tp", "991"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := PublishL2TPAccountingIncident(); err == nil {
				t.Fatal("incident not reported")
			}
			var count int64
			if err := database.GetDB().Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("shared record absent: %d %v", count, err)
			}
		case "check":
			if _, err := os.Stat(l2tpAccountingIncidentPath()); !os.IsNotExist(err) {
				t.Fatalf("reader not isolated: %v", err)
			}
			if err := checkL2TPAccountingIncident(database.GetDB()); err == nil {
				t.Fatal("reader missed shared L2TP incident")
			}
			unlock, err := LockResellerAllocation()
			if err != nil {
				t.Fatalf("traffic lock blocked: %v", err)
			}
			unlock()
		default:
			t.Fatalf("unknown helper %q", mode)
		}
		return
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
	if err := db.AutoMigrate(&model.ResellerNodeCounterIncident{}); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ?", -2).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("isolated PostgreSQL database already has L2TP incident")
	}
	t.Cleanup(func() { _ = db.Where("node_id = ?", -2).Delete(&model.ResellerNodeCounterIncident{}).Error })
	root := t.TempDir()
	for _, phase := range []string{"write", "check"} {
		dir := filepath.Join(root, phase)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestL2TPIncidentAcrossPostgresProcesses$")
		cmd.Env = append(os.Environ(), "PHOENIX_L2TP_PG_HELPER="+phase, "XUI_DB_FOLDER="+dir, "XUI_BIN_FOLDER="+filepath.Join(dir, "bin"))
		output, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("helper %s: %v\n%s", phase, err, output)
		}
	}
}
