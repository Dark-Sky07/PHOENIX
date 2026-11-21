package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Dark-Sky07/phoenix/internal/config"
)

// XrayTrafficJournalDir is local to this panel's configured database folder.
// Other hosts' independent journal directories are not inspected here.
func XrayTrafficJournalDir() string {
	return filepath.Join(config.GetDBFolderPath(), "xray-traffic-journal")
}

func xrayJournalPresent() (bool, error) {
	entries, err := os.ReadDir(XrayTrafficJournalDir())
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot inspect Xray traffic journal: %w", err)
	}
	// Unknown files and interrupted temp writes are not silently ignored.
	return len(entries) != 0, nil
}

func pendingXrayJournalError() error {
	present, err := xrayJournalPresent()
	if err != nil {
		return err
	}
	if present {
		return errors.New("Xray traffic journal requires recovery or manual reconciliation")
	}
	return nil
}
