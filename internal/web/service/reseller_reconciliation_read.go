package service

import "strings"

// isResellerReconciliationError distinguishes evidence gaps that must be
// shown as warnings in read-only reseller views from genuine failures (for
// example database or query errors) that must still fail the request.
//
// The classification is intentionally narrow: every marker below is emitted
// by an explicit reconciliation guard in this package. A caller that releases
// quota or changes runtime state must never use this helper to continue.
func isResellerReconciliationError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	markers := []string{
		"counter incident",
		"manual reconciliation",
		"reconciliation required",
		"fallback requires",
		"unexpected xray exit",
		"baseline missing or invalid",
		"baseline ambiguous",
		"poll requires journal recovery",
	}
	for _, marker := range markers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
