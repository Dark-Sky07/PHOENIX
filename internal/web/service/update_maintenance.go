package service

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/Dark-Sky07/phoenix/internal/config"
)

// updateMaintenanceMarker is written by update.sh before it stops x-ui and is
// removed only after the replacement service has passed stable Xray API
// verification. It lives beside the database rather than beneath the panel
// installation so an atomic directory replacement cannot hide it from either
// generation of the process.
const updateMaintenanceMarker = "panel-update-in-progress"

func updateMaintenanceMarkerPath() string {
	return filepath.Join(config.GetDBFolderPath(), updateMaintenanceMarker)
}

// UpdateMaintenanceActive reports whether a transactional panel replacement is
// in progress or was interrupted. Treat an unreadable, non-regular, or stale
// marker as active: stopping traffic enforcement is safer than interpreting an
// unknown accounting interval as zero. Only the updater clears the marker
// after the newly started runtime is proven stable.
func UpdateMaintenanceActive() bool {
	_, err := os.Lstat(updateMaintenanceMarkerPath())
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	// A marker of any filesystem type is an active barrier. In particular, a
	// malformed file or a symlink must not turn an interrupted update into a
	// green light for accounting enforcement.
	return true
}
