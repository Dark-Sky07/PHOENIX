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

// TuicTrafficJournalDir contains locally retained shutdown deltas; independent
// hosts' directories remain invisible to this local check.
func TuicTrafficJournalDir() string {
	return filepath.Join(config.GetDBFolderPath(), "tuic-traffic-journal")
}

func trafficJournalPresent(dir, label string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot inspect %s traffic journal: %w", label, err)
	}
	// Unknown files and interrupted temp writes are not silently ignored.
	return len(entries) != 0, nil
}

func xrayJournalPresent() (bool, error) {
	return trafficJournalPresent(XrayTrafficJournalDir(), "Xray")
}

func tuicJournalPresent() (bool, error) {
	return trafficJournalPresent(TuicTrafficJournalDir(), "TUIC")
}

func pendingXrayJournalError() error {
	present, err := xrayJournalPresent()
	if err != nil {
		return err
	}
	if present {
		return errors.New("Xray traffic journal requires recovery or manual reconciliation")
	}
	present, err = tuicJournalPresent()
	if err != nil {
		return err
	}
	if present {
		return errors.New("TUIC traffic journal requires recovery or manual reconciliation")
	}
	return nil
}
