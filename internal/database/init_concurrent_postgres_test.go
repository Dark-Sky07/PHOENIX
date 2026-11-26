package database

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestInitDBConcurrentHelper(t *testing.T) {
	if os.Getenv("PHOENIX_INITDB_HELPER") != "1" {
		t.Skip("PostgreSQL concurrent InitDB helper")
	}
	t.Setenv("XUI_DB_TYPE", "postgres")
	t.Setenv("XUI_DB_DSN", os.Getenv("PHOENIX_INITDB_DSN"))
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := InitDB(""); err != nil {
		t.Fatal(err)
	}
	if err := CloseDB(); err != nil {
		t.Fatal(err)
	}
}

// Four independent panel processes bootstrapping one fresh PostgreSQL schema
// must serialize their catalog changes and seeding. This is the race that
// produced `pg_type_typname_nsp_index` duplicate-key errors during an update.
func TestInitDBConcurrentPostgres(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	schema := "phoenix_init_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Exec(fmt.Sprintf("CREATE SCHEMA %s", schema)).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema)).Error })
	schemaDSN := dsn
	if strings.Contains(schemaDSN, "?") {
		schemaDSN += "&search_path=" + schema
	} else {
		schemaDSN += "?search_path=" + schema
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(self, "-test.run=^TestInitDBConcurrentHelper$")
			cmd.Env = append(os.Environ(),
				"PHOENIX_INITDB_HELPER=1",
				"PHOENIX_INITDB_DSN="+schemaDSN,
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				results <- fmt.Errorf("%v: %s", err, out)
				return
			}
			results <- nil
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent InitDB: %v", err)
		}
	}
	var count int64
	if err := admin.Table(schema + ".users").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one seeded admin across four processes, got %d", count)
	}
}
