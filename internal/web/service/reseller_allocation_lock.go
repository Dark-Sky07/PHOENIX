package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
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
// process and, on PostgreSQL, with other processes using the same database.
// SQLite still uses an in-process mutex; this is not a fencing mechanism for
// a lost PostgreSQL connection or a substitute for atomic creation.
func LockResellerAllocation() (func(), error) {
	resellerAllocationMu.Lock()
	db := database.GetDB()
	if db == nil {
		resellerAllocationMu.Unlock()
		return nil, errors.New("database is not available")
	}
	if db.Dialector.Name() != "postgres" {
		return resellerAllocationMu.Unlock, nil
	}
	unlock, ok, err := acquirePostgresAllocationLock(db, true)
	if err != nil || !ok {
		resellerAllocationMu.Unlock()
		if err == nil {
			err = errors.New("allocation lock unavailable")
		}
		return nil, fmt.Errorf("acquire reseller allocation lock: %w", err)
	}
	return func() { unlock(); resellerAllocationMu.Unlock() }, nil
}

// tryLockResellerAllocation never waits on another allocation while the
// traffic writer holds its transaction. Renewal can be deferred to a later tick.
func tryLockResellerAllocation() (func(), error) {
	if !resellerAllocationMu.TryLock() {
		return nil, nil
	}
	db := database.GetDB()
	if db == nil {
		resellerAllocationMu.Unlock()
		return nil, errors.New("database is not available")
	}
	if db.Dialector.Name() != "postgres" {
		return resellerAllocationMu.Unlock, nil
	}
	unlock, ok, err := acquirePostgresAllocationLock(db, false)
	if err != nil || !ok {
		resellerAllocationMu.Unlock()
		return nil, err
	}
	return func() { unlock(); resellerAllocationMu.Unlock() }, nil
}
