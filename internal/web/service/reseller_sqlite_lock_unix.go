//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package service

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockAllocationFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errSQLiteAllocationBusy
	}
	return err
}
func unlockAllocationFile(file *os.File) error { return unix.Flock(int(file.Fd()), unix.LOCK_UN) }
