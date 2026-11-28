package controller

import (
	"strings"
	"sync"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	loginLimitMaxFailures = 5
	loginLimitWindow      = 5 * time.Minute
	loginLimitCooldown    = 15 * time.Minute
)

var defaultLoginLimiter = newLoginLimiter(loginLimitMaxFailures, loginLimitWindow, loginLimitCooldown)

// loginLimiter keeps an in-memory fallback for startup/test availability and
// persists its normal state once the database is open. The database record is
// deliberately keyed by a digest, so an attacker who obtains a DB backup does
// not get a ready-made IP/username login-attempt index.
type loginLimiter struct {
	mu          sync.Mutex
	now         func() time.Time
	maxFailures int
	window      time.Duration
	cooldown    time.Duration
	attempts    map[string]*loginLimitRecord
}

type loginLimitRecord struct {
	failures     []time.Time
	blockedUntil time.Time
}

func newLoginLimiter(maxFailures int, window, cooldown time.Duration) *loginLimiter {
	return &loginLimiter{
		now:         time.Now,
		maxFailures: maxFailures,
		window:      window,
		cooldown:    cooldown,
		attempts:    make(map[string]*loginLimitRecord),
	}
}

func (l *loginLimiter) allow(ip, username string) (time.Time, bool) {
	if blockedUntil, allowed, handled := l.persistentAllow(ip, username); handled {
		return blockedUntil, allowed
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	key := loginLimitKey(ip, username)
	record := l.attempts[key]
	if record == nil {
		return time.Time{}, true
	}
	now := l.now()
	if now.Before(record.blockedUntil) {
		return record.blockedUntil, false
	}
	record.blockedUntil = time.Time{}
	record.failures = pruneLoginFailures(record.failures, now.Add(-l.window))
	if len(record.failures) == 0 {
		delete(l.attempts, key)
	}
	return time.Time{}, true
}

func (l *loginLimiter) registerFailure(ip, username string) (time.Time, bool) {
	if blockedUntil, blocked, handled := l.persistentFailure(ip, username); handled {
		return blockedUntil, blocked
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	key := loginLimitKey(ip, username)
	record := l.attempts[key]
	if record == nil {
		record = &loginLimitRecord{}
		l.attempts[key] = record
	}
	now := l.now()
	record.failures = pruneLoginFailures(record.failures, now.Add(-l.window))
	record.failures = append(record.failures, now)
	if len(record.failures) >= l.maxFailures {
		record.failures = nil
		record.blockedUntil = now.Add(l.cooldown)
		return record.blockedUntil, true
	}
	return time.Time{}, false
}

func (l *loginLimiter) registerSuccess(ip, username string) {
	if db := database.GetDB(); db != nil {
		if err := db.Where("key_hash = ?", loginProtectionKey(ip, username)).Delete(&model.LoginProtection{}).Error; err == nil {
			return
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, loginLimitKey(ip, username))
}

func (l *loginLimiter) persistentAllow(ip, username string) (time.Time, bool, bool) {
	db := database.GetDB()
	if db == nil {
		return time.Time{}, false, false
	}
	var state model.LoginProtection
	if err := db.Where("key_hash = ?", loginProtectionKey(ip, username)).First(&state).Error; err != nil {
		if database.IsNotFound(err) {
			return time.Time{}, true, true
		}
		return time.Time{}, false, false
	}
	now := l.now().UnixMilli()
	if state.BlockedUntil > now {
		return time.UnixMilli(state.BlockedUntil), false, true
	}
	// Records age out after the failure window. This also keeps the table
	// bounded without a separate pruning worker.
	if state.WindowStartedAt == 0 || state.WindowStartedAt <= l.now().Add(-l.window).UnixMilli() {
		_ = db.Where("id = ?", state.ID).Delete(&model.LoginProtection{}).Error
	} else if state.BlockedUntil != 0 {
		_ = db.Model(&model.LoginProtection{}).Where("id = ?", state.ID).Update("blocked_until", 0).Error
	}
	return time.Time{}, true, true
}

func (l *loginLimiter) persistentFailure(ip, username string) (time.Time, bool, bool) {
	db := database.GetDB()
	if db == nil {
		return time.Time{}, false, false
	}
	key := loginProtectionKey(ip, username)
	nowTime := l.now()
	now := nowTime.UnixMilli()
	var until time.Time
	var blocked bool
	err := db.Transaction(func(tx *gorm.DB) error {
		// Establishing the row before SELECT ... FOR UPDATE makes concurrent
		// failures for a new key converge on one durable counter.
		seed := model.LoginProtection{KeyHash: key}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		var state model.LoginProtection
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("key_hash = ?", key).First(&state).Error; err != nil {
			return err
		}
		if state.BlockedUntil > now {
			until, blocked = time.UnixMilli(state.BlockedUntil), true
			return nil
		}
		if state.WindowStartedAt == 0 || state.WindowStartedAt <= nowTime.Add(-l.window).UnixMilli() {
			state.FailureCount = 0
			state.WindowStartedAt = now
			state.BlockedUntil = 0
		}
		state.FailureCount++
		if state.FailureCount >= l.maxFailures {
			state.FailureCount = 0
			state.WindowStartedAt = now
			state.BlockedUntil = nowTime.Add(l.cooldown).UnixMilli()
			until, blocked = time.UnixMilli(state.BlockedUntil), true
		}
		return tx.Save(&state).Error
	})
	if err != nil {
		return time.Time{}, false, false
	}
	return until, blocked, true
}

func loginLimitKey(ip, username string) string {
	return strings.TrimSpace(ip) + "\x00" + strings.ToLower(strings.TrimSpace(username))
}

func loginProtectionKey(ip, username string) string {
	return crypto.HashTokenSHA256(loginLimitKey(ip, username))
}

func pruneLoginFailures(failures []time.Time, cutoff time.Time) []time.Time {
	keepFrom := 0
	for keepFrom < len(failures) && failures[keepFrom].Before(cutoff) {
		keepFrom++
	}
	return failures[keepFrom:]
}
