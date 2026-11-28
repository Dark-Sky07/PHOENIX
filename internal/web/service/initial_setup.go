package service

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"
	"github.com/Dark-Sky07/phoenix/internal/util/random"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// InitialSetupService provisions a first administrator without ever shipping a
// shared default credential. It deliberately becomes inert as soon as a user
// exists, which makes adding it to an existing installation non-destructive.
type InitialSetupService struct{}

const (
	initialSetupTokenLength = 48
	initialSetupTokenTTL    = 30 * time.Minute
	initialSetupTokenFile   = "initial-setup-token"
	initialSetupStateID     = uint(1)
)

func initialSetupTokenPath() string {
	return filepath.Join(config.GetDBFolderPath(), initialSetupTokenFile)
}

// TokenFilePath is intentionally the only way an operator obtains a bootstrap
// token. The token is never returned by an HTTP endpoint or put in a URL, so
// reverse-proxy/request logs and browser history cannot disclose it.
func (s InitialSetupService) TokenFilePath() string { return initialSetupTokenPath() }

func (s InitialSetupService) Required() (bool, error) {
	var count int64
	if err := database.GetDB().Model(&model.User{}).Count(&count).Error; err != nil {
		return false, err
	}
	return count == 0, nil
}

// Ensure creates the local, short-lived first-admin token for a brand-new
// panel. It never creates, rotates, removes or otherwise modifies anything on
// an installation that already has a user. The returned value is only for the
// local installer/operator process; callers must not expose it over HTTP.
func (s InitialSetupService) Ensure() (string, error) {
	required, err := s.Required()
	if err != nil || !required {
		return "", err
	}

	db := database.GetDB()
	var result string
	err = db.Transaction(func(tx *gorm.DB) error {
		// A singleton row prevents multiple panel processes from minting
		// separately-valid links. ON CONFLICT works for SQLite and PostgreSQL.
		seed := model.InitialSetup{ID: initialSetupStateID, ExpiresAt: 0, Completed: false}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"})
		var state model.InitialSetup
		if err := q.First(&state, initialSetupStateID).Error; err != nil {
			return err
		}
		if state.Completed {
			return nil
		}
		now := time.Now().UnixMilli()
		if state.ExpiresAt > now {
			if token, readErr := readInitialSetupToken(); readErr == nil && tokenMatches(state.TokenHash, token) {
				result = token
				return nil
			}
		}

		// A missing or damaged 0600 local token file is recoverable only while
		// the panel is still unconfigured. Rotate the digest inside this locked
		// transaction, then atomically replace the protected local file.
		token := random.Seq(initialSetupTokenLength)
		state.TokenHash = crypto.HashTokenSHA256(token)
		state.ExpiresAt = time.Now().Add(initialSetupTokenTTL).UnixMilli()
		state.Completed = false
		if err := tx.Save(&state).Error; err != nil {
			return err
		}
		if err := writeInitialSetupToken(token); err != nil {
			return err
		}
		result = token
		return nil
	})
	return result, err
}

func readInitialSetupToken() (string, error) {
	contents, err := os.ReadFile(initialSetupTokenPath())
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(initialSetupTokenPath()); err != nil || info.Mode().Perm()&0077 != 0 {
		if err == nil {
			err = errors.New("initial setup token file permissions are unsafe")
		}
		return "", err
	}
	token := strings.TrimSpace(string(contents))
	if token == "" {
		return "", errors.New("initial setup token file is empty")
	}
	return token, nil
}

func writeInitialSetupToken(token string) error {
	path := initialSetupTokenPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".initial-setup-token-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := fmt.Fprintln(tmp, token); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func tokenMatches(hash, token string) bool {
	return subtle.ConstantTimeCompare([]byte(crypto.HashTokenSHA256(strings.TrimSpace(token))), []byte(hash)) == 1
}

func (s InitialSetupService) ValidateToken(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	var state model.InitialSetup
	if err := database.GetDB().First(&state, initialSetupStateID).Error; err != nil || state.Completed || state.ExpiresAt <= time.Now().UnixMilli() {
		return false
	}
	return tokenMatches(state.TokenHash, token)
}

// Complete atomically claims the singleton token and creates exactly one
// superadmin. The conditional state update is the concurrency boundary: two
// browser submissions cannot both win, on either SQLite or PostgreSQL.
func (s InitialSetupService) Complete(token, username, password string) error {
	username = strings.TrimSpace(username)
	if len(username) < 3 || len(username) > 64 {
		return errors.New("username must be 3 to 64 characters")
	}
	if len(password) < 12 || len(password) > 256 {
		return errors.New("password must be 12 to 256 characters")
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("setup token is required")
	}
	hash, err := crypto.HashPasswordAsBcrypt(password)
	if err != nil {
		return err
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.User{}).Count(&count).Error; err != nil {
			return err
		}
		if count != 0 {
			return errors.New("initial setup is already complete")
		}
		now := time.Now().UnixMilli()
		result := tx.Model(&model.InitialSetup{}).
			Where("id = ? AND completed = ? AND expires_at > ? AND token_hash = ?", initialSetupStateID, false, now, crypto.HashTokenSHA256(strings.TrimSpace(token))).
			Update("completed", true)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("setup token is invalid or expired")
		}
		return tx.Create(&model.User{Username: username, Password: hash, Role: "superadmin"}).Error
	})
}

func (s InitialSetupService) Finish() { _ = os.Remove(initialSetupTokenPath()) }
