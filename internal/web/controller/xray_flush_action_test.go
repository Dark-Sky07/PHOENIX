package controller

import (
	"errors"
	"testing"
)

func TestFlushBeforeXrayAction(t *testing.T) {
	cause := errors.New("cannot persist traffic")
	actionCalled := false
	err := flushBeforeXrayAction(func() error { return cause }, func() error { actionCalled = true; return nil })
	if !errors.Is(err, cause) || actionCalled {
		t.Fatalf("failed flush must block action: err=%v action=%v", err, actionCalled)
	}
	order := ""
	err = flushBeforeXrayAction(func() error { order += "flush "; return nil }, func() error { order += "action"; return nil })
	if err != nil || order != "flush action" {
		t.Fatalf("unexpected action ordering %q %v", order, err)
	}
}
