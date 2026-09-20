package eval

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// Comparing two suite runs, without overclaiming.
//
// The temptation is to print "A: 63%, B: 70%" and call B better. At three
// repetitions per task that difference is two runs, and two runs is what the
// same configuration produces against itself on a different afternoon. A
// comparison tool that cannot say so will be used to justify changes that did
// nothing, and — worse — to reject changes that helped.
//
// So the aggregate carries a Fisher exact test. The counts are small and
// integral, which is precisely where the normal approximation behind a z-test
// stops being trustworthy, and Fisher is exact at any count rather than
// asymptotically right at large ones. Per task it is not worth testing at all:
// three trials cannot distinguish anything, and the honest output is the raw
// counts with the delta in runs.

// Delta is one task's change between two reports.
type Delta struct {
	Name     string
	Category string

	PassedA, MeasuredA int
	PassedB, MeasuredB int

	TurnsA, TurnsB   float64
	TokensA, TokensB float64

	// OnlyIn is set when a task appears in one report and not the other.
	OnlyIn string
}

// RunDelta is the change in passing runs, normalised for unequal denominators.
func (d Delta) RunDelta() float64 {
	if d.MeasuredA == 0 || d.MeasuredB == 0 {
		return 0
	}
	rateA := float64(d.PassedA) / float64(d.MeasuredA)
	rateB := float64(d.PassedB) / float64(d.MeasuredB)
	return (rateB - rateA) * float64(d.MeasuredB)
}

// Comparison is the whole diff.
type Comparison struct {
	A, B    *Report
	Deltas  []Delta
	Shared  int
	Warning []string

	PassedA, MeasuredA int
	PassedB, MeasuredB int
	PValue             float64
}

// Compare diffs two reports.
func Compare(a, b *Report) *Comparison {
	c := &Comparison{A: a, B: b}

	if a.Config.Model != b.Config.Model {
		// Not a warning that can be waved away: a model difference dominates
		// every harness difference this suite could detect.
		c.Warning = append(c.Warning, fmt.Sprintf(
			"different models (%s vs %s) — this compares models, not harnesses",
			a.Config.Model, b.Config.Model))
	}
	if a.Config.BaseURL != b.Config.BaseURL {
		c.Warning = append(c.Warning, fmt.Sprintf(
			"different endpoints (%s vs %s)", a.Config.BaseURL, b.Config.BaseURL))
	}

	index := func(r *Report) map[string]TaskReport {
		m := make(map[string]TaskReport, len(r.Tasks))
		for _, t := range r.Tasks {
			m[t.Name] = t
		}
		return m
	}
	ia, ib := index(a), index(b)

	names := map[string]bool{}
	for name := range ia {
		names[name] = true
	}
	for name := range ib {
		names[name] = true
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	for _, name := range ordered {
		ta, okA := ia[name]
		tb, okB := ib[name]

		d := Delta{Name: name}
		switch {
		case !okB:
			d.Category, d.OnlyIn = ta.Category, "a"
		case !okA:
			d.Category, d.OnlyIn = tb.Category, "b"
		default:
			c.Shared++
			d.Category = tb.Category
			d.PassedA, d.MeasuredA = ta.Passed, ta.Measured
			d.PassedB, d.MeasuredB = tb.Passed, tb.Measured
			d.TurnsA, d.TurnsB = ta.MeanTurnsToSuccess, tb.MeanTurnsToSuccess
			d.TokensA, d.TokensB = ta.MeanTokens, tb.MeanTokens

			// Only shared tasks count towards the aggregate. Including a task
			// that exists in one report would move the total for a reason that
			// has nothing to do with the change being evaluated.
			c.PassedA += ta.Passed
			c.MeasuredA += ta.Measured
			c.PassedB += tb.Passed
			c.MeasuredB += tb.Measured
		}
		c.Deltas = append(c.Deltas, d)
	}

	if c.Shared == 0 {
		c.Warning = append(c.Warning, "the two reports share no tasks")
	}

	c.PValue = fisherExact(
		c.PassedA, c.MeasuredA-c.PassedA,
		c.PassedB, c.MeasuredB-c.PassedB)
	return c
}

// Significant reports whether the aggregate difference survives a
// conventional threshold. It is not a licence to ignore the counts.
func (c *Comparison) Significant() bool {
	return c.MeasuredA > 0 && c.MeasuredB > 0 && c.PValue < 0.05
}

// Print renders the comparison.
func (c *Comparison) Print(w io.Writer) {
	labelA := reportLabel(c.A, "A")
	labelB := reportLabel(c.B, "B")

	for _, warning := range c.Warning {
		fmt.Fprintf(w, "WARNING: %s\n", warning)
	}

	fmt.Fprintf(w, "\n%-28s  %8s  %8s  %8s   %s\n",
		"TASK", labelA, labelB, "Δ RUNS", "TURNS / TOKENS")
	fmt.Fprintf(w, "%s\n", strings.Repeat("─", 78))

	for _, d := range c.Deltas {
		if d.OnlyIn != "" {
			which := labelA
			if d.OnlyIn == "b" {
				which = labelB
			}
			fmt.Fprintf(w, "%-28s  only in %s — excluded from the total\n", d.Name, which)
			continue
		}
		fmt.Fprintf(w, "%-28s  %8s  %8s  %8s   %s\n",
			d.Name,
			fmt.Sprintf("%d/%d", d.PassedA, d.MeasuredA),
			fmt.Sprintf("%d/%d", d.PassedB, d.MeasuredB),
			deltaCell(d.RunDelta()),
			secondaryCell(d))
	}

	fmt.Fprintf(w, "%s\n", strings.Repeat("─", 78))
	c.printVerdict(w, labelA, labelB)
}

func (c *Comparison) printVerdict(w io.Writer, labelA, labelB string) {
	if c.MeasuredA == 0 || c.MeasuredB == 0 {
		fmt.Fprintln(w, "Not enough measured runs to compare.")
		return
	}

	rateA := float64(c.PassedA) / float64(c.MeasuredA) * 100
	rateB := float64(c.PassedB) / float64(c.MeasuredB) * 100
	fmt.Fprintf(w, "%s  %.0f%% (%d/%d)    %s  %.0f%% (%d/%d)    p = %.3f\n",
		labelA, rateA, c.PassedA, c.MeasuredA,
		labelB, rateB, c.PassedB, c.MeasuredB, c.PValue)

	switch {
	case c.Significant() && c.PassedB > c.PassedA:
		fmt.Fprintf(w, "%s is better, and the difference is unlikely to be chance.\n", labelB)
	case c.Significant():
		fmt.Fprintf(w, "%s is worse, and the difference is unlikely to be chance.\n", labelB)
	default:
		// The common case, and the one the tool exists to report honestly.
		fmt.Fprintf(w, "No detectable difference at this sample size. "+
			"Raising repetitions is the only way to resolve a gap this small.\n")
	}

	if c.MeasuredA+c.MeasuredB < 60 {
		fmt.Fprintf(w, "Note: %d total runs. A suite this size only detects large "+
			"differences; a real 10%% improvement would usually go unnoticed.\n",
			c.MeasuredA+c.MeasuredB)
	}
}

func reportLabel(r *Report, fallback string) string {
	if r.Config.Label != "" {
		return r.Config.Label
	}
	return fallback
}

func deltaCell(delta float64) string {
	switch {
	case math.Abs(delta) < 0.5:
		return "·"
	case delta > 0:
		return fmt.Sprintf("+%.0f", delta)
	default:
		return fmt.Sprintf("%.0f", delta)
	}
}

func secondaryCell(d Delta) string {
	var parts []string
	if d.TurnsA > 0 && d.TurnsB > 0 {
		parts = append(parts, fmt.Sprintf("%.1f→%.1f", d.TurnsA, d.TurnsB))
	}
	if d.TokensA > 0 && d.TokensB > 0 {
		parts = append(parts, fmt.Sprintf("%s→%s",
			humanCount(d.TokensA), humanCount(d.TokensB)))
	}
	return strings.Join(parts, "  ")
}

// fisherExact is the two-sided p-value for a 2x2 table.
//
// Exact rather than approximate because the counts here are small — thirty
// runs a side is typical — and that is where a chi-square or z approximation
// starts reporting significance that is not there.
//
// It sums the hypergeometric probability of every table with the same margins
// that is at least as extreme as the observed one, working in logs so the
// factorials do not overflow.
func fisherExact(a, b, c, d int) float64 {
	if a < 0 || b < 0 || c < 0 || d < 0 {
		return 1
	}
	n := a + b + c + d
	if n == 0 {
		return 1
	}

	rowA, rowB := a+b, c+d
	colPass := a + c

	observed := logHypergeometric(a, rowA, rowB, colPass)
	const tolerance = 1e-9

	total := 0.0
	low := max(0, colPass-rowB)
	high := min(rowA, colPass)
	for k := low; k <= high; k++ {
		p := logHypergeometric(k, rowA, rowB, colPass)
		if p <= observed+tolerance {
			total += math.Exp(p)
		}
	}
	return math.Min(total, 1)
}

// logHypergeometric is log P(k passes in group A | fixed margins).
func logHypergeometric(k, rowA, rowB, colPass int) float64 {
	return logChoose(rowA, k) +
		logChoose(rowB, colPass-k) -
		logChoose(rowA+rowB, colPass)
}

func logChoose(n, k int) float64 {
	if k < 0 || k > n {
		return math.Inf(-1)
	}
	return logFactorial(n) - logFactorial(k) - logFactorial(n-k)
}

func logFactorial(n int) float64 {
	v, _ := math.Lgamma(float64(n) + 1)
	return v
}
