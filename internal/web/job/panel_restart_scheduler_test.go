package job

import (
	"testing"

	"github.com/robfig/cron/v3"
)

func TestPanelRestartSchedulerConfigureReplaceAndDisable(t *testing.T) {
	c := cron.New(cron.WithSeconds())
	scheduler := &PanelRestartScheduler{}

	if err := scheduler.Configure(c, true, "@every 6h"); err != nil {
		t.Fatalf("configure initial schedule: %v", err)
	}
	if got := len(c.Entries()); got != 1 {
		t.Fatalf("entries after enabling = %d, want 1", got)
	}

	if err := scheduler.Configure(c, true, "@daily"); err != nil {
		t.Fatalf("replace schedule: %v", err)
	}
	if got := len(c.Entries()); got != 1 {
		t.Fatalf("entries after replacing = %d, want 1", got)
	}

	if err := scheduler.Configure(c, true, "@every 30m"); err == nil {
		t.Fatal("expected too-frequent schedule to be rejected")
	}
	if got := len(c.Entries()); got != 1 {
		t.Fatalf("entries after rejected update = %d, want existing entry preserved", got)
	}

	if err := scheduler.Configure(c, false, ""); err != nil {
		t.Fatalf("disable schedule: %v", err)
	}
	if got := len(c.Entries()); got != 0 {
		t.Fatalf("entries after disabling = %d, want 0", got)
	}
}
