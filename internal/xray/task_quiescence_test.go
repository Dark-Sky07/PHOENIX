package xray

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/buf"
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

type delayedCounterReader struct {
	release  <-chan struct{}
	finished chan<- struct{}
}

func (r *delayedCounterReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	<-r.release
	result := buf.MergeBytes(nil, []byte("late-data"))
	close(r.finished)
	return result, nil
}

type testAtomicCounter struct{ value atomic.Int64 }

func (c *testAtomicCounter) Value() int64      { return c.value.Load() }
func (c *testAtomicCounter) Set(v int64) int64 { return c.value.Swap(v) }
func (c *testAtomicCounter) Add(v int64) int64 { return c.value.Add(v) - v }

// DispatchLink wraps the user uplink in TimeoutWrapperReader. After a timeout,
// its background read can finish while the corresponding bytes still have not
// been applied to the counter; the next read adds them. No wall-clock pause or
// pair of identical polls can prove this pending read was drained.
func TestXrayTimedOutReadCanAccountBytesLater(t *testing.T) {
	release := make(chan struct{})
	finished := make(chan struct{})
	counter := &testAtomicCounter{}
	reader := &buf.TimeoutWrapperReader{Reader: &delayedCounterReader{release: release, finished: finished}, Counter: counter}
	initial, err := reader.ReadMultiBufferTimeout(10 * time.Millisecond)
	if err != nil || len(initial) != 0 {
		t.Fatalf("expected timed-out empty read, got %v %v", initial, err)
	}
	if counter.Value() != 0 {
		t.Fatal("counter changed before underlying read finished")
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("underlying read did not finish")
	}
	if counter.Value() != 0 {
		t.Fatal("counter changed before buffered read was consumed")
	}
	result, err := reader.ReadMultiBuffer()
	if err != nil {
		t.Fatal(err)
	}
	defer buf.ReleaseMulti(result)
	if got := counter.Value(); got != int64(len("late-data")) {
		t.Fatalf("late counter value = %d", got)
	}
}
