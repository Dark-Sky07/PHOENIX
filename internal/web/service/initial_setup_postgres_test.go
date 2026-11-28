package service

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestInitialSetupPostgresLifecycle exercises the same first-admin path used
// in production against a real PostgreSQL server. It is opt-in so ordinary
// developer test runs do not require a database service.
func TestInitialSetupPostgresLifecycle(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	schema := "phoenix_setup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec(fmt.Sprintf("CREATE SCHEMA %s", schema)).Error; err != nil {
		t.Fatal(err)
	}
	defer func() { _ = admin.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema)).Error }()

	schemaDSN := dsn
	if strings.Contains(schemaDSN, "?") {
		schemaDSN += "&search_path=" + schema
	} else {
		schemaDSN += "?search_path=" + schema
	}
	t.Setenv("XUI_DB_TYPE", "postgres")
	t.Setenv("XUI_DB_DSN", schemaDSN)
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := database.InitDB(""); err != nil {
		t.Fatalf("InitDB PostgreSQL: %v", err)
	}
	defer func() { _ = database.CloseDB() }()

	setup := InitialSetupService{}
	needed, err := setup.Required()
	if err != nil || !needed {
		t.Fatalf("fresh PostgreSQL setup required = (%v, %v), want (true, nil)", needed, err)
	}
	token, err := setup.Ensure()
	if err != nil {
		t.Fatalf("Ensure PostgreSQL: %v", err)
	}
	if err := setup.Complete(token, "pg-first-admin", "a-long-first-password"); err != nil {
		t.Fatalf("Complete PostgreSQL: %v", err)
	}
	setup.Finish()

	var user model.User
	if err := database.GetDB().Where("username = ?", "pg-first-admin").First(&user).Error; err != nil {
		t.Fatalf("load PostgreSQL first administrator: %v", err)
	}
	if user.Role != "superadmin" || !crypto.CheckPasswordHash(user.Password, "a-long-first-password") {
		t.Fatalf("unexpected PostgreSQL first administrator: %#v", user)
	}
	if token, err := setup.Ensure(); err != nil || token != "" {
		t.Fatalf("upgrade must not create another PostgreSQL setup token: (%q, %v)", token, err)
	}
}
