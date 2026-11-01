package job

import (
	"errors"
	"strings"
	"sync"

	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/web/entity"
	"github.com/Dark-Sky07/phoenix/internal/web/global"
	"github.com/robfig/cron/v3"
)

// PanelRestartScheduler owns the optional scheduled restart entry for one web
// server instance. The restart hook only reloads the panel and subscription
// servers; it does not stop or restart Xray.
type PanelRestartScheduler struct {
	mu      sync.Mutex
	cron    *cron.Cron
	entryID cron.EntryID
}

// Configure replaces or removes the scheduled panel restart. The schedule is
// validated before changing the active entry so a bad update cannot silently
// discard a working schedule.
func (s *PanelRestartScheduler) Configure(scheduler *cron.Cron, enabled bool, expression string) error {
	if scheduler == nil {
		if enabled {
			return errors.New("panel restart scheduler is not initialized")
		}
		return nil
	}
	if enabled {
		expression = strings.TrimSpace(expression)
		if err := entity.ValidatePanelAutoRestartCron(expression); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cron != scheduler {
		s.cron = scheduler
		s.entryID = 0
	}
	if !enabled {
		if s.entryID != 0 {
			scheduler.Remove(s.entryID)
			s.entryID = 0
		}
		logger.Info("scheduled panel restart disabled")
		return nil
	}

	newEntryID, err := scheduler.AddFunc(expression, func() {
		logger.Warning("scheduled panel restart: restarting panel and subscription services")
		if !global.TriggerRestart() {
			logger.Error("scheduled panel restart skipped: restart hook is not registered")
		}
	})
	if err != nil {
		return err
	}
	oldEntryID := s.entryID
	s.entryID = newEntryID
	if oldEntryID != 0 {
		scheduler.Remove(oldEntryID)
	}
	logger.Infof("scheduled panel restart configured: %s", expression)
	return nil
}
