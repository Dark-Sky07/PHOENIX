//go:build linux

package pppvpn

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Never block on a FIFO substituted for a session record. O_NOFOLLOW rejects
// a final symlink, and fstat validates the opened inode, not just its name.
func openPPPRecord(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("pppvpn: session record %q is not a regular file", name)
	}
	file, err := root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("pppvpn: session record %q is not a regular file", name)
	}
	return file, nil
}

func openPPPDirectory(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
}
