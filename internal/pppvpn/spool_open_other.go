//go:build !linux

package pppvpn

import (
	"fmt"
	"os"
)

// PPP runtime is Linux-specific. On other platforms reject known special files
// before opening; this fallback does not protect against concurrent replacement.
func openPPPRecord(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("pppvpn: session record %q is not a regular file", name)
	}
	file, err := root.Open(name)
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
	info, err := root.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("pppvpn: %q is not a directory", name)
	}
	return root.Open(name)
}
