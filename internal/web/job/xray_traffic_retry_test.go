package job

import (
	"errors"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestXrayTrafficPendingBatchRetainedUntilPersisted(t *testing.T) {
	batch := &xrayTrafficBatch{id: "batch", clients: []*xray.ClientTraffic{{Email: "test", Up: 42}}}
	j := &XrayTrafficJob{pending: batch}
	fail := errors.New("commit status unknown")
	if _, _, err := j.applyPending(func(got *xrayTrafficBatch) (bool, bool, error) {
		if got != batch {
			t.Fatal("different batch")
		}
		return false, false, fail
	}); !errors.Is(err, fail) {
		t.Fatalf("got error %v", err)
	}
	if j.pending != batch || j.pending.clients[0].Up != 42 {
		t.Fatal("failed batch was lost or altered")
	}
	if _, _, err := j.applyPending(func(got *xrayTrafficBatch) (bool, bool, error) {
		if got != batch || got.id != "batch" {
			t.Fatal("retry changed batch identity")
		}
		return false, false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if j.pending != nil {
		t.Fatal("successful batch retained")
	}
}
