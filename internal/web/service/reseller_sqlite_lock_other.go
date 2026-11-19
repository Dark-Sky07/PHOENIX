//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package service

import (
	"errors"
	"os"
)

func lockAllocationFile(*os.File) error {
	return errors.New("SQLite cross-process allocation lock is unsupported on this platform")
}
func unlockAllocationFile(*os.File) error { return nil }
