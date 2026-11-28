package service

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"
)

func initInitialSetupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
}

func TestInitialSetupFreshPanelCreatesNoDefaultAccount(t *testing.T) {
	initInitialSetupTestDB(t)
	setup := InitialSetupService{}
	needed, err := setup.Required()
	if err != nil || !needed {
		t.Fatalf("fresh panel required = (%v, %v), want (true, nil)", needed, err)
	}
	token, err := setup.Ensure()
	if err != nil || len(token) != initialSetupTokenLength {
		t.Fatalf("Ensure = (%q, %v)", token, err)
	}
	info, err := os.Stat(setup.TokenFilePath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("token file mode = %v, err = %v; want 0600", info.Mode().Perm(), err)
	}
	if !setup.ValidateToken(token) {
		t.Fatal("new token must validate")
	}
	if err := setup.Complete(token, "first-admin", "a-long-first-password"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	setup.Finish()
	var user model.User
	if err := database.GetDB().Where("username = ?", "first-admin").First(&user).Error; err != nil {
		t.Fatalf("first admin missing: %v", err)
	}
	if user.Role != "superadmin" || !crypto.CheckPasswordHash(user.Password, "a-long-first-password") {
		t.Fatal("first admin must be a bcrypt-hashed superadmin")
	}
	if setup.ValidateToken(token) {
		t.Fatal("consumed token must never validate")
	}
	if _, err := os.Stat(setup.TokenFilePath()); !os.IsNotExist(err) {
		t.Fatalf("consumed token file still exists: %v", err)
	}
}

func TestInitialSetupUpgradePreservesExistingAdministrator(t *testing.T) {
	initInitialSetupTestDB(t)
	original := model.User{Username: "existing-admin", Password: "already-hashed", Role: "superadmin", LoginEpoch: 7}
	if err := database.GetDB().Create(&original).Error; err != nil {
		t.Fatal(err)
	}
	setup := InitialSetupService{}
	needed, err := setup.Required()
	if err != nil || needed {
		t.Fatalf("existing panel required = (%v, %v), want (false, nil)", needed, err)
	}
	if token, err := setup.Ensure(); err != nil || token != "" {
		t.Fatalf("Ensure existing = (%q, %v), want empty nil", token, err)
	}
	var got model.User
	if err := database.GetDB().First(&got, original.Id).Error; err != nil {
		t.Fatal(err)
	}
	if got.Username != original.Username || got.Password != original.Password || got.LoginEpoch != original.LoginEpoch {
		t.Fatalf("upgrade changed existing admin: %#v", got)
	}
	if _, err := os.Stat(setup.TokenFilePath()); !os.IsNotExist(err) {
		t.Fatalf("upgrade must not write setup token: %v", err)
	}
}

func TestInitialSetupExpiredAndConcurrentCompletion(t *testing.T) {
	initInitialSetupTestDB(t)
	setup := InitialSetupService{}
	token, err := setup.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(&model.InitialSetup{}).Where("id = ?", initialSetupStateID).Update("expires_at", time.Now().Add(-time.Minute).UnixMilli()).Error; err != nil {
		t.Fatal(err)
	}
	if setup.ValidateToken(token) {
		t.Fatal("expired token must fail")
	}
	token, err = setup.Ensure()
	if err != nil || !setup.ValidateToken(token) {
		t.Fatalf("Ensure must rotate an expired token: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var successes int
	var mu sync.Mutex
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := setup.Complete(token, "race-admin", "a-long-first-password"); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()
	if successes != 1 {
		t.Fatalf("concurrent setup successes = %d, want exactly 1", successes)
	}
	var count int64
	if err := database.GetDB().Model(&model.User{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("user count = %d, err = %v; want 1", count, err)
	}
}
