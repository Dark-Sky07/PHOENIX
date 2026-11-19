package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/logger"
	"gorm.io/gorm"
)

var errSQLiteAllocationBusy = errors.New("SQLite allocation lock busy")

// SQLite reports the actual main database path, including when InitDB was
// given an explicit path. Anonymous in-memory databases cannot be shared
// between processes, so no filesystem lock is necessary for those.
func sqliteAllocationDatabasePath(db *gorm.DB) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var rows []struct {
		Name string
		File string
	}
	if err := db.WithContext(ctx).Raw("PRAGMA database_list").Scan(&rows).Error; err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Name != "main" {
			continue
		}
		if row.File == "" {
			return "", nil
		}
		path, err := filepath.Abs(row.File)
		if err != nil {
			return "", err
		}
		// Canonicalize symlinks so different path spellings use the same lock.
		return filepath.EvalSymlinks(path)
	}
	return "", errors.New("SQLite main database not found")
}

func acquireSQLiteAllocationLock(db *gorm.DB, wait bool) (func(), bool, error) {
	path, err := sqliteAllocationDatabasePath(db)
	if err != nil {
		return nil, false, err
	}
	if path == "" {
		return func() {}, true, nil
	}
	return acquireSQLiteAllocationFileLock(path, wait)
}

func acquireSQLiteAllocationFileLock(dbPath string, wait bool) (func(), bool, error) {
	file, err := os.OpenFile(dbPath+".reseller-allocation.lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = lockAllocationFile(file)
		if err == nil {
			return func() {
				if err := unlockAllocationFile(file); err != nil {
					logger.Warning("release SQLite reseller allocation lock failed:", err)
				}
				_ = file.Close()
			}, true, nil
		}
		if !errors.Is(err, errSQLiteAllocationBusy) {
			_ = file.Close()
			return nil, false, err
		}
		if !wait {
			_ = file.Close()
			return nil, false, nil
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, false, fmt.Errorf("timed out waiting for SQLite reseller allocation lock")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
