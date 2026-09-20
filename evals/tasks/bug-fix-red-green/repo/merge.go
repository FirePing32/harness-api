package intervals

import "sort"

// Interval is a half-open range [Start, End).
type Interval struct {
	Start, End int
}

// Merge collapses overlapping intervals into the smallest equivalent set.
func Merge(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}

	sorted := append([]Interval(nil), in...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	out := []Interval{sorted[0]}
	for _, next := range sorted[1:] {
		last := &out[len(out)-1]
		if next.Start < last.End {
			last.End = next.End
			continue
		}
		out = append(out, next)
	}
	return out
}
