package service

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestTrafficWriterStartStopStartAcceptsWrites(t *testing.T) {
	resetTrafficWriterForTest(t)

	StartTrafficWriter()
	var writes atomic.Int32
	if err := submitTrafficWrite(func() error {
		writes.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("first submitTrafficWrite: %v", err)
	}

	StopTrafficWriter()
	StartTrafficWriter()
	if err := submitTrafficWrite(func() error {
		writes.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("second submitTrafficWrite: %v", err)
	}

	if got := writes.Load(); got != 2 {
		t.Fatalf("writes = %d, want 2", got)
	}
}

func TestTrafficWriterSubmitAfterStopRunsInline(t *testing.T) {
	resetTrafficWriterForTest(t)

	StartTrafficWriter()
	StopTrafficWriter()

	ran := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- submitTrafficWrite(func() error {
			close(ran)
			return nil
		})
	}()

	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("submitTrafficWrite did not run after traffic writer stopped")
	}
	if err := waitTrafficWriterErr(t, errCh); err != nil {
		t.Fatalf("submitTrafficWrite after stop: %v", err)
	}
}

func TestTrafficWriterStopDrainsQueuedWrite(t *testing.T) {
	resetTrafficWriterForTest(t)

	StartTrafficWriter()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- submitTrafficWrite(func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	waitTrafficWriterSignal(t, firstStarted, "first write did not start")

	secondRan := make(chan struct{})
	secondErr := make(chan error, 1)
	go func() {
		secondErr <- submitTrafficWrite(func() error {
			close(secondRan)
			return nil
		})
	}()
	waitTrafficWriterQueued(t)

	stopDone := make(chan struct{})
	go func() {
		StopTrafficWriter()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		t.Fatal("StopTrafficWriter returned before in-flight write was released")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	waitTrafficWriterSignal(t, stopDone, "StopTrafficWriter did not return")
	waitTrafficWriterSignal(t, secondRan, "queued write was not drained")

	if err := waitTrafficWriterErr(t, firstErr); err != nil {
		t.Fatalf("first submitTrafficWrite: %v", err)
	}
	if err := waitTrafficWriterErr(t, secondErr); err != nil {
		t.Fatalf("second submitTrafficWrite: %v", err)
	}
}

func TestTrafficWriterConcurrentStopDuringSubmitDoesNotHang(t *testing.T) {
	resetTrafficWriterForTest(t)

	StartTrafficWriter()
	started := make(chan struct{})
	release := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- submitTrafficWrite(func() error {
			close(started)
			<-release
			return nil
		})
	}()
	waitTrafficWriterSignal(t, started, "write did not start")

	stopDone := make(chan struct{})
	go func() {
		StopTrafficWriter()
		close(stopDone)
	}()

	close(release)
	waitTrafficWriterSignal(t, stopDone, "StopTrafficWriter hung during submit")
	if err := waitTrafficWriterErr(t, errCh); err != nil {
		t.Fatalf("submitTrafficWrite during stop: %v", err)
	}
}

func resetTrafficWriterForTest(t *testing.T) {
	t.Helper()
	StopTrafficWriter()
	twMu.Lock()
	clearTrafficWriterState()
	twMu.Unlock()
	t.Cleanup(func() {
		StopTrafficWriter()
		twMu.Lock()
		clearTrafficWriterState()
		twMu.Unlock()
	})
}

func waitTrafficWriterQueued(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		twMu.Lock()
		queued := 0
		if twQueue != nil {
			queued = len(twQueue)
		}
		twMu.Unlock()
		if queued > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("write was not queued")
}

func waitTrafficWriterSignal(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal(msg)
	}
}

func waitTrafficWriterErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for traffic writer result")
		return nil
	}
}

func TestTrafficWriterInlineWritesRemainSerialized(t *testing.T) {
	resetTrafficWriterForTest(t)
	started := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() { first <- submitTrafficWrite(func() error { close(started); <-release; return nil }) }()
	waitTrafficWriterSignal(t, started, "inline write did not start")
	secondStarted := make(chan struct{})
	second := make(chan error, 1)
	go func() { second <- submitTrafficWrite(func() error { close(secondStarted); return nil }) }()
	select {
	case <-secondStarted:
		t.Fatal("inline writes ran concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := waitTrafficWriterErr(t, first); err != nil {
		t.Fatal(err)
	}
	if err := waitTrafficWriterErr(t, second); err != nil {
		t.Fatal(err)
	}
	waitTrafficWriterSignal(t, secondStarted, "second inline write did not run")
}

func TestTrafficWriterStoppingDoesNotOvertakeQueuedWrites(t *testing.T) {
	resetTrafficWriterForTest(t)
	StartTrafficWriter()
	started, release := make(chan struct{}), make(chan struct{})
	first, queued, afterStop := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() { first <- submitTrafficWrite(func() error { close(started); <-release; return nil }) }()
	waitTrafficWriterSignal(t, started, "first write did not start")
	queuedRan := make(chan struct{})
	go func() { queued <- submitTrafficWrite(func() error { close(queuedRan); return nil }) }()
	waitTrafficWriterQueued(t)
	stopped := make(chan struct{})
	go func() { StopTrafficWriter(); close(stopped) }()
	// Observe cancellation rather than racing the stopping goroutine.
	twMu.Lock()
	ctx := twCtx
	twMu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel writer")
	}
	go func() {
		afterStop <- submitTrafficWrite(func() error {
			select {
			case <-queuedRan:
				return nil
			default:
				return fmt.Errorf("inline write overtook queued work")
			}
		})
	}()
	close(release)
	waitTrafficWriterSignal(t, stopped, "writer did not stop")
	for _, ch := range []<-chan error{first, queued, afterStop} {
		if err := waitTrafficWriterErr(t, ch); err != nil {
			t.Fatal(err)
		}
	}
}
