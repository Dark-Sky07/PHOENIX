package service

import (
	"math"
	"reflect"
	"testing"
)

func TestChunkHelpersHandleExtremeSizes(t *testing.T) {
	ints := []int{1, 2, 3, 4, 5}
	strings := []string{"a", "b", "c", "d", "e"}
	cases := []struct {
		size    int
		wantInt [][]int
		wantStr [][]string
	}{
		{0, nil, nil},
		{-1, nil, nil},
		{1, [][]int{{1}, {2}, {3}, {4}, {5}}, [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}}},
		{2, [][]int{{1, 2}, {3, 4}, {5}}, [][]string{{"a", "b"}, {"c", "d"}, {"e"}}},
		{5, [][]int{{1, 2, 3, 4, 5}}, [][]string{{"a", "b", "c", "d", "e"}}},
		{math.MaxInt, [][]int{{1, 2, 3, 4, 5}}, [][]string{{"a", "b", "c", "d", "e"}}},
	}
	for _, tc := range cases {
		if got := chunkInts(ints, tc.size); !reflect.DeepEqual(got, tc.wantInt) {
			t.Errorf("ints size %d: got %v want %v", tc.size, got, tc.wantInt)
		}
		if got := chunkStrings(strings, tc.size); !reflect.DeepEqual(got, tc.wantStr) {
			t.Errorf("strings size %d: got %v want %v", tc.size, got, tc.wantStr)
		}
	}
	if chunkInts(nil, math.MaxInt) != nil || chunkStrings(nil, math.MaxInt) != nil {
		t.Fatal("empty input should return nil")
	}
}
