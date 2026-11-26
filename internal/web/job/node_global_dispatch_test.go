package job

import (
	"errors"
	"testing"
)

func TestNodeGlobalDispatchVerifiedNeverDowngrades(t *testing.T) {
	failure := errors.New("receiver or provisioning failed")
	verifiedCalls, legacyCalls := 0, 0
	verified := func() error { verifiedCalls++; return failure }
	legacy := func() error { legacyCalls++; return nil }
	if err := dispatchNodeGlobalTraffic(true, verified, legacy); !errors.Is(err, failure) {
		t.Fatalf("verified failure: %v", err)
	}
	if verifiedCalls != 1 || legacyCalls != 0 {
		t.Fatalf("unexpected downgrade: %d/%d", verifiedCalls, legacyCalls)
	}
	if err := dispatchNodeGlobalTraffic(false, verified, legacy); err != nil {
		t.Fatal(err)
	}
	if verifiedCalls != 1 || legacyCalls != 1 {
		t.Fatalf("legacy dispatch: %d/%d", verifiedCalls, legacyCalls)
	}
}
