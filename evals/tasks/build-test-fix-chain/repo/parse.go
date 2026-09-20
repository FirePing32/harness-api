package pipeline

import (
	"strconv"
	"strings"
)

// ParseRow splits "name:qty" and returns the pieces.
func ParseRow(row string) (string, int, error) {
	parts := strings.Split(row, ":")
	qty, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return "", 0, err
	}
	return strings.TrimSpace(parts[0]), qty, nil
}

// Total sums the quantities of every well-formed row, skipping the rest.
func Total(rows []string) int {
	sum := 0
	for _, row := range rows {
		_, qty, err := ParseRow(row)
		if err != nil {
			continue
		}
		sum += qty
	}
	return sum
}
