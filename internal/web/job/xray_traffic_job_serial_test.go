package job

import (
	"sync/atomic"
	"testing"
)

func TestXrayTrafficJobSkipsOverlappingPollUntilPersistenceCompletes(t *testing.T) {
	j := NewXrayTrafficJob()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		defer close(done)
		j.runOnce(func() {
			calls.Add(1)
			close(started)
			<-release // simulate a poll blocked on persistence
		})
	}()
	<-started
	j.runOnce(func() { calls.Add(1) })
	if got := calls.Load(); got != 1 {
		t.Fatalf("overlapping poll ran: got %d calls", got)
	}
	close(release)
	<-done
	j.runOnce(func() { calls.Add(1) })
	if got := calls.Load(); got != 2 {
		t.Fatalf("later poll did not run: got %d calls", got)
	}
}
