package pipeline

import "testing"

func TestTotalSkipsMalformedRows(t *testing.T) {
	rows := []string{"bolt: 3", "nut: 4", "missing-colon", "washer: 5"}
	if got := Total(rows); got != 12 {
		t.Errorf("Total = %d, want 12", got)
	}
}
