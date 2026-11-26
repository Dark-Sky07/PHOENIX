package web

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/web/job"
	"github.com/robfig/cron/v3"
)

func TestXrayTrafficJobScheduledSynchronouslyOnReplacement(t *testing.T) {
	for i := 0; i < 2; i++ {
		c := cron.New(cron.WithSeconds())
		if err := scheduleXrayTrafficJob(c); err != nil {
			t.Fatal(err)
		}
		entries := c.Entries()
		if len(entries) != 1 || entries[0].Job != job.SharedXrayTrafficJob() {
			t.Fatalf("scheduler %d does not contain shared job: %+v", i, entries)
		}
	}
}
