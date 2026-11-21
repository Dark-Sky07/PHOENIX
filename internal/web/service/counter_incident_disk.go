package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func counterIncidentDir() string {
	return filepath.Join(config.GetDBFolderPath(), "counter-incident-fallback")
}

func syncCounterIncidentDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Fallback files are never automatically cleared: even an incomplete file
// blocks participating operations after restart until reviewed by an operator.
// A database-backed incident is still preferred and is independently checked.
func storeCounterIncidentFallback(incident model.ResellerNodeCounterIncident) error {
	dir := counterIncidentDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := syncCounterIncidentDir(filepath.Dir(dir)); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", incident.NodeId, transferEmailKey(incident.Email))))
	path := filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
	data, err := json.Marshal(incident)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return syncCounterIncidentDir(dir)
	}
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return syncCounterIncidentDir(dir)
}

func diskCounterIncidentError() error {
	entries, err := os.ReadDir(counterIncidentDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot inspect counter incident fallback: %w", err)
	}
	if len(entries) > 0 {
		return errors.New("counter incident fallback requires manual reconciliation")
	}
	return nil
}
