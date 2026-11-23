//go:build linux

package job

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// O_NONBLOCK and O_NOFOLLOW prevent a substituted FIFO or final symlink from
// stalling recovery or redirecting reads; fstat checks the opened inode.
func openXrayJournalRecord(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("journal entry %q is not a regular file", name)
	}
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("journal entry %q is not a regular file", name)
	}
	return f, nil
}
