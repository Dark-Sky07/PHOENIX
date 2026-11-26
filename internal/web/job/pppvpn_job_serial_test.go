package job

import (
	"sync/atomic"
	"testing"
)

func TestPPPVPNJobsSerializeAcrossInstances(t *testing.T) {
	first, second := NewPPPVPNJob(), NewPPPVPNJob()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	go func() { defer close(done); first.runOnce(func() { calls.Add(1); close(started); <-release }) }()
	<-started
	second.runOnce(func() { calls.Add(1) })
	if calls.Load() != 1 {
		t.Fatal("second job entered while first collector was processing")
	}
	close(release)
	<-done
	second.runOnce(func() { calls.Add(1) })
	if calls.Load() != 2 {
		t.Fatal("second job did not resume after first completed")
	}
}
