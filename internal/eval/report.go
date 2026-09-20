package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// Turning runs into numbers, and being careful about which numbers are real.
//
// Two decisions here are worth stating, because both could reasonably have
// gone the other way and both change what the headline figure means.
//
// A run that never reached the model is excluded from the denominator rather
// than counted as a failure. A rate-limited afternoon would otherwise look
// exactly like a capability regression, and the two call for opposite
// responses. Every printed rate carries its denominator so the exclusion is
// visible rather than assumed.
//
// Mean iterations is computed over successful runs only. Averaging in a run
// that gave up at the iteration ceiling would make a harness that fails fast
// look more efficient than one that succeeds slowly, which is backwards.

// Config identifies what produced a report. Comparing two reports taken
// against different models measures the models, so the comparison prints this
// and refuses to pretend otherwise.
type Config struct {
	BaseURL     string `json:"base_url"`
	Model       string `json:"model"`
	Label       string `json:"label,omitempty"`
	Repetitions int    `json:"repetitions"`
}

// TaskReport is every repetition of one task, plus its aggregates.
type TaskReport struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Runs     []Run  `json:"runs"`

	Attempts int `json:"attempts"`
	Measured int `json:"measured"`
	Passed   int `json:"passed"`
	Errored  int `json:"errored"`

	// PassRate is over measured runs. -1 when nothing was measured, which is
	// distinct from 0 and must not be averaged with it.
	PassRate float64 `json:"pass_rate"`

	// MeanTurnsToSuccess is 0 when nothing succeeded.
	MeanTurnsToSuccess float64 `json:"mean_turns_to_success"`
	MeanTokens         float64 `json:"mean_tokens"`
	MeanDurationMS     float64 `json:"mean_duration_ms"`

	Tools ToolStats `json:"tools,omitempty"`
}

// Report is a whole suite run.
type Report struct {
	StartedAt  string       `json:"started_at"`
	DurationMS int64        `json:"duration_ms"`
	Config     Config       `json:"config"`
	Tasks      []TaskReport `json:"tasks"`

	Attempts int `json:"attempts"`
	Measured int `json:"measured"`
	Passed   int `json:"passed"`
	Errored  int `json:"errored"`

	PassRate float64   `json:"pass_rate"`
	Tools    ToolStats `json:"tools,omitempty"`
}

// Summarise computes a task's aggregates from its runs.
func Summarise(t *Task, runs []Run) TaskReport {
	tr := TaskReport{
		Name: t.Name, Category: t.Category, Runs: runs,
		Attempts: len(runs), PassRate: -1, Tools: ToolStats{},
	}

	var (
		successTurns int
		tokens       int
		duration     int64
	)
	for _, r := range runs {
		if r.Errored() {
			tr.Errored++
			continue
		}
		tr.Measured++
		tokens += r.Usage.TotalTokens
		duration += r.DurationMS
		if r.Passed {
			tr.Passed++
			successTurns += r.Turns
		}
		tr.Tools.merge(r.Tools)
	}

	if tr.Measured > 0 {
		tr.PassRate = float64(tr.Passed) / float64(tr.Measured)
		tr.MeanTokens = float64(tokens) / float64(tr.Measured)
		tr.MeanDurationMS = float64(duration) / float64(tr.Measured)
	}
	if tr.Passed > 0 {
		tr.MeanTurnsToSuccess = float64(successTurns) / float64(tr.Passed)
	}
	return tr
}

// merge folds one run's tool counts into an aggregate.
func (s ToolStats) merge(other ToolStats) {
	for name, stat := range other {
		agg := s[name]
		if agg == nil {
			agg = &ToolStat{Codes: map[string]int{}}
			s[name] = agg
		}
		agg.Calls += stat.Calls
		agg.Errors += stat.Errors
		for code, n := range stat.Codes {
			agg.Codes[code] += n
		}
	}
}

// ErrorRate is the fraction of calls that returned an error.
func (s *ToolStat) ErrorRate() float64 {
	if s.Calls == 0 {
		return 0
	}
	return float64(s.Errors) / float64(s.Calls)
}

// Finish computes the suite-level aggregates.
func (r *Report) Finish() {
	r.Tools = ToolStats{}
	for _, t := range r.Tasks {
		r.Attempts += t.Attempts
		r.Measured += t.Measured
		r.Passed += t.Passed
		r.Errored += t.Errored
		r.Tools.merge(t.Tools)
	}
	if r.Measured > 0 {
		r.PassRate = float64(r.Passed) / float64(r.Measured)
	} else {
		r.PassRate = -1
	}
}

// Write serialises a report.
func (r *Report) Write(path string) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// LoadReport reads a serialised report.
func LoadReport(path string) (*Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(r.Tasks) == 0 {
		return nil, fmt.Errorf("%s contains no tasks", path)
	}
	return &r, nil
}

// Print renders a report for a terminal.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "\n%s\n", strings.Repeat("─", 72))
	fmt.Fprintf(w, "%-28s  %6s  %7s  %8s  %9s\n",
		"TASK", "PASS", "TURNS", "TOKENS", "TIME")
	fmt.Fprintf(w, "%s\n", strings.Repeat("─", 72))

	byCategory := map[string][]TaskReport{}
	var categories []string
	for _, t := range r.Tasks {
		if _, seen := byCategory[t.Category]; !seen {
			categories = append(categories, t.Category)
		}
		byCategory[t.Category] = append(byCategory[t.Category], t)
	}
	sort.Strings(categories)

	for _, category := range categories {
		fmt.Fprintf(w, "\n%s\n", category)
		for _, t := range byCategory[category] {
			fmt.Fprintf(w, "  %-26s  %6s  %7s  %8s  %8.1fs\n",
				t.Name, passCell(t), turnsCell(t),
				humanCount(t.MeanTokens), t.MeanDurationMS/1000)
			for _, line := range distinctFailures(t.Runs) {
				fmt.Fprintf(w, "      ↳ %s\n", line)
			}
		}
	}

	fmt.Fprintf(w, "\n%s\n", strings.Repeat("─", 72))
	if r.Measured == 0 {
		fmt.Fprintf(w, "No runs were measured. %d attempt(s) errored.\n", r.Errored)
	} else {
		fmt.Fprintf(w, "pass@1  %.0f%%  (%d/%d measured runs across %d tasks)\n",
			r.PassRate*100, r.Passed, r.Measured, len(r.Tasks))
	}
	if r.Errored > 0 {
		// Loud, because an unnoticed error count turns an outage into a
		// capability finding.
		fmt.Fprintf(w, "WARNING: %d of %d run(s) never reached the model and are "+
			"excluded from the rate above.\n", r.Errored, r.Attempts)
	}

	r.printTools(w)
}

// printTools is the tool-ergonomics table.
//
// A high error rate on one tool is not a model problem to be prompted away. It
// says that tool's failure messages are not telling the model what to do next,
// and the fix is in that tool's source.
func (r *Report) printTools(w io.Writer) {
	if len(r.Tools) == 0 {
		return
	}
	names := make([]string, 0, len(r.Tools))
	for name := range r.Tools {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprintf(w, "\n%-10s  %7s  %7s  %9s   %s\n",
		"TOOL", "CALLS", "ERRORS", "RATE", "TOP CODES")
	for _, name := range names {
		s := r.Tools[name]
		fmt.Fprintf(w, "%-10s  %7d  %7d  %8.0f%%   %s\n",
			name, s.Calls, s.Errors, s.ErrorRate()*100, topCodes(s.Codes, 3))
	}
}

func topCodes(codes map[string]int, n int) string {
	type kv struct {
		code string
		n    int
	}
	all := make([]kv, 0, len(codes))
	for code, count := range codes {
		all = append(all, kv{code, count})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].n != all[j].n {
			return all[i].n > all[j].n
		}
		return all[i].code < all[j].code
	})

	var parts []string
	for i, e := range all {
		if i == n {
			break
		}
		parts = append(parts, fmt.Sprintf("%s×%d", e.code, e.n))
	}
	return strings.Join(parts, " ")
}

// distinctFailures lists each failure reason once, with how often it occurred.
// Three repetitions failing the same way is one finding; failing three
// different ways is three.
func distinctFailures(runs []Run) []string {
	counts := map[string]int{}
	var order []string
	for _, r := range runs {
		reasons := r.Failures
		if r.Errored() {
			reasons = []string{"error: " + r.Error}
		}
		for _, reason := range reasons {
			if counts[reason] == 0 {
				order = append(order, reason)
			}
			counts[reason]++
		}
	}

	out := make([]string, 0, len(order))
	for _, reason := range order {
		if n := counts[reason]; n > 1 {
			out = append(out, fmt.Sprintf("%s (×%d)", reason, n))
		} else {
			out = append(out, reason)
		}
	}
	return out
}

func passCell(t TaskReport) string {
	if t.Measured == 0 {
		return "  err"
	}
	return fmt.Sprintf("%d/%d", t.Passed, t.Measured)
}

func turnsCell(t TaskReport) string {
	if t.Passed == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f", t.MeanTurnsToSuccess)
}

func humanCount(n float64) string {
	switch {
	case n <= 0:
		return "—"
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", n/1_000)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}
