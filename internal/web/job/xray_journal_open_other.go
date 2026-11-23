//go:build !linux

package job

import (
	"fmt"
	"os"
)

// Reject known special files on other platforms. Unlike the Linux implementation,
// this check cannot prevent a concurrent replacement between Lstat and Open.
func openXrayJournalRecord(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("journal entry %q is not a regular file", name)
	}
	f, err := root.Open(name)
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
