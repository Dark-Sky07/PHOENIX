package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestXrayTrafficBaselineIsSharedForSameManagedProcess(t *testing.T) {
	var state xrayTrafficState
	first := xray.NewProcess(nil)
	second := xray.NewProcess(nil)
	state.observeProcess(first)
	if state.process != first {
		t.Fatal("first process not recorded")
	}
	state.api.StatsLastValues = map[string]int64{"user>>>a>>>traffic>>>uplink": 123}
	// A new caller must not clear the existing process's baseline.
	state.observeProcess(first)
	if state.api.StatsLastValues["user>>>a>>>traffic>>>uplink"] != 123 {
		t.Fatal("same process baseline discarded")
	}
	state.observeProcess(second)
	if state.process != second || len(state.api.StatsLastValues) != 0 {
		t.Fatal("new managed core inherited previous counters")
	}
}
