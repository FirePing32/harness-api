package intervals

import "reflect"
import "testing"

func TestMergeOverlapping(t *testing.T) {
	got := Merge([]Interval{{1, 5}, {2, 3}, {6, 8}})
	want := []Interval{{1, 5}, {6, 8}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %v, want %v", got, want)
	}
}

func TestMergeAdjacent(t *testing.T) {
	got := Merge([]Interval{{1, 3}, {3, 6}})
	want := []Interval{{1, 3}, {3, 6}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Merge = %v, want %v", got, want)
	}
}

func TestMergeEmpty(t *testing.T) {
	if got := Merge(nil); got != nil {
		t.Errorf("Merge(nil) = %v, want nil", got)
	}
}
