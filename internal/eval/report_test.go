package eval

import (
	"math"
	"strings"
	"testing"

	"github.com/FirePing32/harness-api/internal/oai"
)

func run(passed bool, turns, tokens int) Run {
	return Run{
		Passed: passed, Turns: turns,
		Usage: oai.Usage{TotalTokens: tokens}, DurationMS: 1000,
	}
}

func erroredRun() Run { return Run{Error: "connection refused"} }

func TestPassRateExcludesRunsThatNeverReachedTheModel(t *testing.T) {
	// A rate-limited afternoon and a capability regression produce the same
	// number if errors count as failures, and they call for opposite responses.
	task := &Task{Name: "t", Category: "c"}
	got := Summarise(task, []Run{run(true, 4, 100), erroredRun(), run(true, 6, 200)})

	if got.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", got.Attempts)
	}
	if got.Measured != 2 || got.Errored != 1 {
		t.Errorf("Measured = %d, Errored = %d; want 2 and 1", got.Measured, got.Errored)
	}
	if got.PassRate != 1.0 {
		t.Errorf("PassRate = %.2f, want 1.0 — the error is not a failure", got.PassRate)
	}
}

func TestPassRateIsNegativeWhenNothingWasMeasured(t *testing.T) {
	// Distinct from zero, and it must not be averaged with zero.
	got := Summarise(&Task{Name: "t", Category: "c"},
		[]Run{erroredRun(), erroredRun()})

	if got.PassRate != -1 {
		t.Errorf("PassRate = %.2f, want -1 for 'no data'", got.PassRate)
	}
}

func TestMeanTurnsCountsOnlySuccessfulRuns(t *testing.T) {
	// Averaging in a run that gave up at the iteration ceiling would make a
	// harness that fails fast look more efficient than one that succeeds
	// slowly, which is backwards.
	got := Summarise(&Task{Name: "t", Category: "c"}, []Run{
		run(true, 4, 100),
		run(false, 50, 9000), // thrashed, then gave up
		run(true, 6, 200),
	})

	if got.MeanTurnsToSuccess != 5 {
		t.Errorf("MeanTurnsToSuccess = %.1f, want 5 (4 and 6, not the 50)",
			got.MeanTurnsToSuccess)
	}
	// Tokens, by contrast, are averaged over everything measured: a failed run
	// still cost money.
	if got.MeanTokens != 3100 {
		t.Errorf("MeanTokens = %.0f, want 3100 — failures are billed too", got.MeanTokens)
	}
}

func TestMeanTurnsIsZeroWhenNothingSucceeded(t *testing.T) {
	got := Summarise(&Task{Name: "t", Category: "c"}, []Run{run(false, 9, 100)})
	if got.MeanTurnsToSuccess != 0 {
		t.Errorf("MeanTurnsToSuccess = %.1f, want 0", got.MeanTurnsToSuccess)
	}
}

func TestToolStatsAggregateAcrossRuns(t *testing.T) {
	stats := func(calls, errs int) ToolStats {
		return ToolStats{"edit": {Calls: calls, Errors: errs,
			Codes: map[string]int{"no_match": errs}}}
	}
	got := Summarise(&Task{Name: "t", Category: "c"}, []Run{
		{Passed: true, Tools: stats(3, 1)},
		{Passed: false, Tools: stats(5, 4)},
		{Error: "unreachable", Tools: stats(99, 99)}, // must not be counted
	})

	edit := got.Tools["edit"]
	if edit.Calls != 8 || edit.Errors != 5 {
		t.Errorf("edit: %d calls, %d errors; want 8 and 5", edit.Calls, edit.Errors)
	}
	if edit.Codes["no_match"] != 5 {
		t.Errorf("codes = %v", edit.Codes)
	}
}

func TestReportPrintsItsDenominatorAndWarnsAboutErrors(t *testing.T) {
	// An unnoticed error count turns an outage into a capability finding.
	r := &Report{Tasks: []TaskReport{
		Summarise(&Task{Name: "a", Category: "c"}, []Run{run(true, 3, 10), erroredRun()}),
	}}
	r.Finish()

	var out strings.Builder
	r.Print(&out)
	text := out.String()

	if !strings.Contains(text, "1/1 measured runs") {
		t.Errorf("the denominator is not stated:\n%s", text)
	}
	if !strings.Contains(text, "WARNING") || !strings.Contains(text, "never reached the model") {
		t.Errorf("the error count is not called out:\n%s", text)
	}
}

func TestReportGroupsByCategory(t *testing.T) {
	// A suite that passes overall while every refactor fails is a different
	// situation from one that fails evenly, and the aggregate hides it.
	r := &Report{Tasks: []TaskReport{
		Summarise(&Task{Name: "b", Category: "02-refactor"}, []Run{run(false, 9, 10)}),
		Summarise(&Task{Name: "a", Category: "01-edit"}, []Run{run(true, 3, 10)}),
	}}
	r.Finish()

	var out strings.Builder
	r.Print(&out)
	text := out.String()

	edit := strings.Index(text, "01-edit")
	refactor := strings.Index(text, "02-refactor")
	if edit < 0 || refactor < 0 || edit > refactor {
		t.Errorf("categories missing or out of order:\n%s", text)
	}
}

func TestDistinctFailuresCollapseRepeats(t *testing.T) {
	// Three repetitions failing the same way is one finding; three different
	// ways is three.
	got := distinctFailures([]Run{
		{Failures: []string{"check failed (exit 1)"}},
		{Failures: []string{"check failed (exit 1)"}},
		{Failures: []string{"took 22 turns, budget was 8"}},
	})

	if len(got) != 2 {
		t.Fatalf("got %d distinct failures, want 2: %v", len(got), got)
	}
	if !strings.Contains(got[0], "×2") {
		t.Errorf("the repeat is not counted: %q", got[0])
	}
}

func TestFisherExactMatchesKnownValues(t *testing.T) {
	// Checked against R's fisher.test. The whole point of the comparison
	// verdict rests on this being right, and "it looked plausible" is not a
	// standard a significance test can be held to.
	cases := []struct {
		name       string
		a, b, c, d int
		want       float64
		tol        float64
	}{
		// A table with no difference at all.
		{"identical", 15, 15, 15, 15, 1.0, 1e-9},
		// The textbook tea-tasting table.
		{"tea tasting", 3, 1, 1, 3, 0.4857142857, 1e-6},
		// A clear separation.
		{"complete separation", 10, 0, 0, 10, 1.0825e-5, 1e-8},
		// The cases this suite actually produces: 30 runs a side. Both look
		// like wins in a table and neither is one — a six-run gap at n=30 is
		// still p = 0.16, which is the whole reason the verdict is hedged.
		{"20/30 vs 24/30", 20, 10, 24, 6, 0.38165412, 1e-8},
		{"18/30 vs 24/30", 18, 12, 24, 6, 0.15806205, 1e-8},
		{"empty", 0, 0, 0, 0, 1.0, 1e-9},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fisherExact(c.a, c.b, c.c, c.d)
			if math.Abs(got-c.want) > c.tol {
				t.Errorf("fisherExact(%d,%d,%d,%d) = %.8f, want %.8f",
					c.a, c.b, c.c, c.d, got, c.want)
			}
		})
	}
}

func TestComparisonRefusesToCallASmallDifferenceReal(t *testing.T) {
	// The failure this tool exists to prevent: 20/30 against 24/30 looks like
	// a clear win and is four runs, which the same configuration produces
	// against itself on a different afternoon.
	a := reportOf("baseline", 10, 2) // 10 tasks, 2 of 3 passing
	b := reportOf("candidate", 10, 2)
	b.Tasks[0] = Summarise(&Task{Name: "task00", Category: "c"},
		[]Run{run(true, 3, 10), run(true, 3, 10), run(true, 3, 10)})
	b.Tasks[1] = Summarise(&Task{Name: "task01", Category: "c"},
		[]Run{run(true, 3, 10), run(true, 3, 10), run(true, 3, 10)})
	a.Finish()
	b.Finish()

	c := Compare(a, b)
	if c.Significant() {
		t.Errorf("a two-run difference was called significant (p = %.3f)", c.PValue)
	}

	var out strings.Builder
	c.Print(&out)
	if !strings.Contains(out.String(), "No detectable difference") {
		t.Errorf("the verdict overclaims:\n%s", out.String())
	}
}

func TestComparisonDetectsALargeRealDifference(t *testing.T) {
	a := reportOf("baseline", 10, 0)  // nothing passes
	b := reportOf("candidate", 10, 3) // everything passes
	a.Finish()
	b.Finish()

	c := Compare(a, b)
	if !c.Significant() {
		t.Errorf("a 0/30 to 30/30 difference was not detected (p = %.4f)", c.PValue)
	}

	var out strings.Builder
	c.Print(&out)
	if !strings.Contains(out.String(), "candidate is better") {
		t.Errorf("the verdict does not name the winner:\n%s", out.String())
	}
}

func TestComparisonWarnsWhenTheModelsDiffer(t *testing.T) {
	// A model difference dominates every harness difference this suite could
	// detect, so the comparison is meaningless and has to say so.
	a := reportOf("a", 3, 1)
	b := reportOf("b", 3, 2)
	a.Config.Model = "gpt-4.1"
	b.Config.Model = "claude-sonnet-4-6"
	a.Finish()
	b.Finish()

	c := Compare(a, b)
	joined := strings.Join(c.Warning, " ")
	if !strings.Contains(joined, "different models") {
		t.Errorf("no warning about mismatched models: %v", c.Warning)
	}
	if !strings.Contains(joined, "compares models, not harnesses") {
		t.Errorf("the warning does not say what the comparison actually measures: %v", c.Warning)
	}
}

func TestComparisonExcludesTasksMissingFromEitherSide(t *testing.T) {
	// Including a task that only one report has would move the total for a
	// reason unrelated to the change being evaluated.
	a := reportOf("a", 2, 3)
	b := reportOf("b", 3, 3) // one task more
	a.Finish()
	b.Finish()

	c := Compare(a, b)
	if c.Shared != 2 {
		t.Errorf("Shared = %d, want 2", c.Shared)
	}
	if c.MeasuredA != 6 || c.MeasuredB != 6 {
		t.Errorf("measured %d vs %d; the extra task leaked into the total",
			c.MeasuredA, c.MeasuredB)
	}

	var out strings.Builder
	c.Print(&out)
	if !strings.Contains(out.String(), "only in") {
		t.Errorf("the unmatched task is not flagged:\n%s", out.String())
	}
}

func TestComparisonSaysWhenTheSuiteIsTooSmall(t *testing.T) {
	a := reportOf("a", 3, 2)
	b := reportOf("b", 3, 3)
	a.Finish()
	b.Finish()

	var out strings.Builder
	Compare(a, b).Print(&out)
	if !strings.Contains(out.String(), "only detects large") {
		t.Errorf("a nine-run-a-side suite did not warn about its own power:\n%s", out.String())
	}
}

// reportOf builds a report with n tasks, each with 3 runs of which the given
// number pass.
func reportOf(label string, tasks, passing int) *Report {
	r := &Report{Config: Config{Label: label, Model: "m", BaseURL: "u", Repetitions: 3}}
	for i := range tasks {
		name := "task0" + string(rune('0'+i))
		runs := make([]Run, 0, 3)
		for j := range 3 {
			runs = append(runs, run(j < passing, 4, 100))
		}
		r.Tasks = append(r.Tasks, Summarise(&Task{Name: name, Category: "c"}, runs))
	}
	return r
}
