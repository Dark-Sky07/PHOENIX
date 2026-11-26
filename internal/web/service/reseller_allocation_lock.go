package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"gorm.io/gorm"
)

// All participating panel processes sharing a PostgreSQL database use this
// session advisory lock. It covers quota checks and their subsequent writes,
// which currently span several database transactions and runtime calls.
const resellerAllocationAdvisoryKey int64 = 0x50484f454e4958

func acquirePostgresAllocationLock(db *gorm.DB, wait bool) (func(), bool, error) {
	pool, err := db.DB()
	if err != nil {
		return nil, false, err
	}
	timeout := 10 * time.Second
	if !wait {
		timeout = 250 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := pool.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if wait {
		// pg_advisory_lock returns void; execute rather than interpreting it.
		_, err = conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", resellerAllocationAdvisoryKey)
		acquired = err == nil
	} else {
		err = conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", resellerAllocationAdvisoryKey).Scan(&acquired)
	}
	if err != nil || !acquired {
		if err != nil {
			// A canceled lock query may have raced with acquisition; never
			// return an uncertain session to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
		return nil, false, err
	}
	return func() { releasePostgresAllocationLock(conn) }, true, nil
}

func releasePostgresAllocationLock(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var released bool
	err := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", resellerAllocationAdvisoryKey).Scan(&released)
	if err != nil || !released {
		logger.Warning("release reseller allocation advisory lock failed:", err)
		// Do not put a potentially still-locked connection back in the pool.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = conn.Close()
}

// LockResellerAllocation coordinates participating allocation paths in this
// process and with participating processes sharing the database. SQLite uses
// a sidecar file lock and PostgreSQL uses a session advisory lock. Neither is
// a substitute for atomic creation or fencing a lost database connection.
func LockResellerAllocation() (func(), error) {
	return lockResellerAllocation(false)
}

// LockResellerAllocationForXrayRecovery is reserved for replaying Xray
// journals while in-flight markers deliberately block normal mutations.
func LockResellerAllocationForXrayRecovery() (func(), error) {
	return lockResellerAllocation(true)
}

func lockResellerAllocation(recovery bool) (func(), error) {
	resellerAllocationMu.Lock()
	db := database.GetDB()
	if db == nil {
		resellerAllocationMu.Unlock()
		return nil, errors.New("database is not available")
	}
	var unlock func()
	var ok bool
	var err error
	switch db.Dialector.Name() {
	case "postgres":
		unlock, ok, err = acquirePostgresAllocationLock(db, true)
	case "sqlite":
		unlock, ok, err = acquireSQLiteAllocationLock(db, true)
	default:
		err = errors.New("unsupported database for reseller allocation locking")
	}
	if err != nil || !ok {
		resellerAllocationMu.Unlock()
		if err == nil {
			err = errors.New("allocation lock unavailable")
		}
		return nil, fmt.Errorf("acquire reseller allocation lock: %w", err)
	}
	if !recovery {
		if err := pendingXrayPollError(db); err != nil {
			unlock()
			resellerAllocationMu.Unlock()
			return nil, err
		}
		if err := pendingXrayJournalError(); err != nil {
			unlock()
			resellerAllocationMu.Unlock()
			return nil, err
		}
	}
	return func() { unlock(); resellerAllocationMu.Unlock() }, nil
}

// pendingXrayPollError must be checked after acquiring the shared allocation
// lock. It is global because the affected identities may not yet be captured.
func pendingXrayPollError(db *gorm.DB) error {
	if db == nil {
		return errors.New("database unavailable while checking Xray poll markers")
	}
	var count int64
	if err := db.Model(&model.XrayTrafficInFlight{}).Count(&count).Error; err != nil {
		return fmt.Errorf("check Xray poll markers: %w", err)
	}
	if count != 0 {
		return errors.New("Xray traffic poll requires journal recovery or manual reconciliation")
	}
	return nil
}

// tryLockResellerAllocation never waits on another allocation while the
// traffic writer holds its transaction. Renewal can be deferred to a later tick.
func tryLockResellerAllocation(tx *gorm.DB) (func(), error) {
	if !resellerAllocationMu.TryLock() {
		return nil, nil
	}
	db := database.GetDB()
	if db == nil {
		resellerAllocationMu.Unlock()
		return nil, errors.New("database is not available")
	}
	var unlock func()
	var ok bool
	var err error
	switch db.Dialector.Name() {
	case "postgres":
		unlock, ok, err = acquirePostgresAllocationLock(db, false)
	case "sqlite":
		// Use the existing writer transaction for PRAGMA: a second pooled
		// connection may be unavailable while that transaction is open.
		unlock, ok, err = acquireSQLiteAllocationLock(tx, false)
	default:
		err = errors.New("unsupported database for reseller allocation locking")
	}
	if err != nil || !ok {
		resellerAllocationMu.Unlock()
		return nil, err
	}
	if err := pendingXrayPollError(tx); err != nil {
		unlock()
		resellerAllocationMu.Unlock()
		return nil, err
	}
	if err := pendingXrayJournalError(); err != nil {
		unlock()
		resellerAllocationMu.Unlock()
		return nil, err
	}
	return func() { unlock(); resellerAllocationMu.Unlock() }, nil
}
