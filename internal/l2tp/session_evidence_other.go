//go:build !linux

package l2tp

import (
	"fmt"
	"os"
)

// L2TP is Linux-specific. These checks are not race-free against replacement
// on other systems; a trusted private runtime directory is still required.
func openEvidenceDir(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("L2TP path %q is not a directory", name)
	}
	return root.Open(name)
}

func openEvidenceRecord(root *os.Root, name string) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("L2TP marker %q is not a regular file", name)
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
		return nil, fmt.Errorf("L2TP marker %q is not a regular file", name)
	}
	return f, nil
}
