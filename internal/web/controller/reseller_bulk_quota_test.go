package controller

import (
	"math"
	"testing"
)

func TestResellerBulkAllocationRejectsOverflow(t *testing.T) {
	for _, tc := range []struct {
		count   int
		bytes   int64
		want    int64
		invalid bool
	}{
		{2, 50, 100, false},
		{1, math.MaxInt64, math.MaxInt64, false},
		{2, math.MaxInt64, 0, true},
		{2, math.MinInt64, 0, true},
		{-1, 1, 0, true},
	} {
		got, err := resellerBulkAllocation(tc.count, tc.bytes)
		if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
			t.Fatalf("count=%d bytes=%d: got %d, %v", tc.count, tc.bytes, got, err)
		}
	}
}
