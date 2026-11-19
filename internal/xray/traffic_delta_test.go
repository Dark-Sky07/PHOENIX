package xray

import "testing"

func TestTrafficCounterDeltaNewClientsAndReset(t *testing.T) {
	api := &XrayAPI{}
	if _, ok := api.trafficCounterDelta("user>>>existing>>>traffic>>>uplink", 100); ok {
		t.Fatal("initial bootstrap must not duplicate previous core traffic")
	}
	api.statsBaselineReady = true
	if got, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", 75); !ok || got != 75 {
		t.Fatalf("first interval lost: %d %v", got, ok)
	}
	if got, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", 90); !ok || got != 15 {
		t.Fatalf("subsequent delta: %d %v", got, ok)
	}
	if got, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", 12); !ok || got != 12 {
		t.Fatalf("reset interval lost: %d %v", got, ok)
	}
	if _, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", -1); ok {
		t.Fatal("negative counter accepted")
	}
	if got, ok := api.trafficCounterDelta("user>>>new>>>traffic>>>uplink", 14); !ok || got != 2 {
		t.Fatalf("negative counter changed baseline: %d %v", got, ok)
	}
}

func TestTrafficBaselineResetOnNewCore(t *testing.T) {
	api := &XrayAPI{}
	api.trafficCounterDelta("user>>>alice>>>traffic>>>uplink", 200)
	api.statsBaselineReady = true
	api.ResetTrafficBaseline(true)
	if got, ok := api.trafficCounterDelta("user>>>alice>>>traffic>>>uplink", 500); !ok || got != 500 {
		t.Fatalf("new core inherited stale baseline: got %d include %v", got, ok)
	}
	api.ResetTrafficBaseline(false)
	if _, ok := api.trafficCounterDelta("user>>>alice>>>traffic>>>uplink", 800); ok {
		t.Fatal("reattaching to an existing core must establish a baseline")
	}
}
