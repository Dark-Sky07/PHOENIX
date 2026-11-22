package xray

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/task"
)

// A returned inbound handler is not a final counter barrier: Xray's task.Run
// can return on one failed task while a sibling is still running. Other async
// dispatcher, UDP and mux work may outlive a handler as well.
func TestXrayTaskRunMayReturnBeforeSiblingCompletes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	result := make(chan error, 1)
	defer close(release)
	go func() {
		result <- task.Run(context.Background(), func() error {
			close(started)
			<-release
			close(finished)
			return nil
		}, func() error {
			<-started
			return errors.New("injected task failure")
		})
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected task error")
		}
	case <-time.After(time.Second):
		t.Fatal("task.Run did not return on failure")
	}
	select {
	case <-finished:
		t.Fatal("sibling unexpectedly finished before release")
	default:
	}
}
