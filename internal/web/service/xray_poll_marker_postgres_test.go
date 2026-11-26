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
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Run only with an isolated PostgreSQL test database. The children use
// separate panel folders and exercise the actual allocation entry point.
func TestXrayPollMarkerAcrossPostgresProcesses(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	mode := os.Getenv("PHOENIX_POLL_MARKER_HELPER")
	if mode != "" {
		t.Setenv("XUI_DB_TYPE", "postgres")
		t.Setenv("XUI_DB_DSN", dsn)
		if err := database.InitDB(""); err != nil {
			t.Fatal(err)
		}
		defer database.CloseDB()
		if mode == "recovery" || mode == "exclusive" || mode == "nonexclusive" {
			unlock, err := LockResellerAllocationForXrayRecovery()
			if err != nil {
				t.Fatalf("recovery lock blocked by marker: %v", err)
			}
			defer unlock()
			id := os.Getenv("PHOENIX_POLL_MARKER_ID")
			if err := VerifyXrayPollMarker(id); err != nil {
				t.Fatalf("shared marker unavailable: %v", err)
			}
			if mode == "exclusive" {
				if err := VerifyExclusiveXrayPollMarker(id); err != nil {
					t.Fatalf("sole marker rejected: %v", err)
				}
			}
			if mode == "nonexclusive" {
				if err := VerifyExclusiveXrayPollMarker(id); err == nil {
					t.Fatal("foreign unresolved marker accepted")
				}
			}
			return
		}
		unlock, err := LockResellerAllocation()
		if mode == "blocked" {
			if err == nil {
				unlock()
				t.Fatal("shared poll marker did not block allocation")
			}
			return
		}
		if mode != "clear" || err != nil {
			t.Fatalf("allocation after recovery: %v", err)
		}
		unlock()
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
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&model.XrayTrafficInFlight{}); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if err := db.Create(&model.XrayTrafficInFlight{ID: id}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Where("id = ?", id).Delete(&model.XrayTrafficInFlight{}).Error })
	run := func(mode string) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), mode)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestXrayPollMarkerAcrossPostgresProcesses$")
		cmd.Env = append(os.Environ(), "PHOENIX_POLL_MARKER_HELPER="+mode, "PHOENIX_POLL_MARKER_ID="+id, "XUI_DB_FOLDER="+dir)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child %s: %v\n%s", mode, err, output)
		}
	}
	run("blocked")
	run("recovery")
	run("exclusive")
	foreign := uuid.NewString()
	if err := db.Create(&model.XrayTrafficInFlight{ID: foreign}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Where("id = ?", foreign).Delete(&model.XrayTrafficInFlight{}).Error })
	run("nonexclusive")
	if err := db.Where("id = ?", foreign).Delete(&model.XrayTrafficInFlight{}).Error; err != nil {
		t.Fatal(err)
	}
	run("exclusive")
	if err := db.Where("id = ?", id).Delete(&model.XrayTrafficInFlight{}).Error; err != nil {
		t.Fatal(err)
	}
	run("clear")
}
