package sliceutil

import (
	"reflect"
	"testing"
)

func TestFilter(t *testing.T) {
	slice := []int{1, 2, 3, 4, 5}
	filtered := Filter(slice, func(v int) bool {
		return v%2 == 0
	})
	want := []int{2, 4}
	if !reflect.DeepEqual(filtered, want) {
		t.Errorf("Filter(%v) = %v, want %v", slice, filtered, want)
	}
}

func TestMap(t *testing.T) {
	slice := []int{1, 2, 3, 4, 5}
	mapped := Map(slice, func(v int) int {
		return v * 2
	})
	want := []int{2, 4, 6, 8, 10}
	if !reflect.DeepEqual(mapped, want) {
		t.Errorf("Map(%v) = %v, want %v", slice, mapped, want)
	}
}
