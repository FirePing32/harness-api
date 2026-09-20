package main

import (
	"fmt"
	"os"
	"strings"
)

type Row struct {
	Name  string
	Count int
}

func format(rows []Row) string {
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("%-10s %d\n", r.Name, r.Count))
	}
	return b.String()
}

func total(rows []Row) int {
	sum := 0
	for _, r := range rows {
		sum += r.Count
	}
	return sum
}

func main() {
	rows := []Row{{"alpha", 3}, {"beta", 7}}

	fmt.Print(format(rows))
	fmt.Printf("total: %d\n", total(rows, true))

	if len(rows) == 0 {
		os.Exit(1)
	}
	fmt.Println(strconv.Itoa(len(rows)) + " rows")
}
