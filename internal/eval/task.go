// Package eval measures whether the harness actually works.
//
// Everything else in this repository is an argument: read-before-edit should
// reduce bad edits, instructive tool errors should reduce retries, pruning
// before summarising should preserve more of what matters. Arguments are
// cheap. This package is the part that can say whether any of them are true,
// and — more usefully — whether a change made things worse.
//
// Three choices shape the design.
//
// It runs against a live server over HTTP, as an ordinary OpenAI client would.
// Testing the loop in-process would be faster and would miss everything the
// server does: binding, streaming, quirk transforms, the guard chain. The
// thing being measured is the whole system, so the whole system is what runs.
//
// Checks are programs, not model judgements. An LLM judge would let a task
// grade prose, which is tempting and wrong: judges disagree with themselves
// across runs, and a regression detector that is itself noisy detects noise.
// A check either exits zero or it does not.
//
// Task specs are JSON rather than the YAML the plan called for. YAML would
// read slightly better and would cost a dependency; at this size the trade is
// not worth it. The one real loss is comments, so specs carry a "notes" field
// that says what the task is probing and why, which is the thing a comment
// would have said anyway.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Duration is a time.Duration that reads as "90s" in JSON rather than as a
// count of nanoseconds nobody can check by eye.
type Duration time.Duration

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"90s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON writes the duration back in the same form.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// D is the underlying duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Task is one thing the agent is asked to do, and the definition of having
// done it.
type Task struct {
	// Name identifies the task and must match its directory.
	Name string `json:"name"`

	// Category groups tasks in the report. A suite that passes overall while
	// every refactor task fails is a different situation from one that fails
	// evenly, and the aggregate number hides the difference.
	Category string `json:"category"`

	// Prompt is sent as the user message, verbatim. It is deliberately not
	// templated: a prompt that varies between runs is not a fixed measurement.
	Prompt string `json:"prompt"`

	// Notes says what this task is probing and why it is worth a slot. Not
	// used by the runner; it exists because a task nobody can justify is a
	// task that will be silently wrong for a year.
	Notes string `json:"notes,omitempty"`

	// Repetitions overrides the suite default. Agent runs are stochastic, so a
	// single pass is an anecdote.
	Repetitions int `json:"repetitions,omitempty"`

	// Timeout bounds one repetition.
	Timeout Duration `json:"timeout,omitempty"`

	// MaxTurns fails the run if the agent took more tool-calling turns than
	// this, even when the check passes. A task solved in thirty turns that
	// could be solved in three is a harness problem that a pass/fail column
	// will never show.
	MaxTurns int `json:"max_turns,omitempty"`

	// Tools restricts the toolset, for probes that target one tool. Empty
	// means the full set.
	Tools []string `json:"tools,omitempty"`

	// ExpectNoChanges fails the run if the agent modified anything. This is
	// how an impossible request is checked without asking a model whether a
	// refusal sounded like a refusal: the observable property of a correct
	// refusal is that nothing was invented.
	ExpectNoChanges bool `json:"expect_no_changes,omitempty"`

	// MustSurvive names files whose deletion or truncation fails the run
	// regardless of anything else. Every destructive agent failure looks
	// reasonable in the transcript; it is only visible in the filesystem.
	MustSurvive []string `json:"must_survive,omitempty"`

	// Check is the checker script, relative to the task directory.
	Check string `json:"check,omitempty"`

	// Dir is where the task was loaded from. Not serialised.
	Dir string `json:"-"`
}

const (
	defaultCheck = "check.sh"
	repoDir      = "repo"

	// hiddenDir holds files withheld from the agent and applied by the checker
	// afterwards. The leading underscore is load-bearing: Go tooling skips
	// directories named that way, and without it a hidden Go test compiles as
	// part of this module and breaks `go vet ./...`. It cannot be excluded with
	// its own go.mod either, since it has to compile inside whichever workspace
	// copies it in.
	hiddenDir = "_hidden"

	defaultTimeout = 10 * time.Minute
)

// RepoPath is the template workspace copied fresh for each repetition.
func (t *Task) RepoPath() string { return filepath.Join(t.Dir, repoDir) }

// CheckPath is the checker script.
func (t *Task) CheckPath() string { return filepath.Join(t.Dir, t.Check) }

// TimeoutOr returns the task's timeout, or the fallback.
func (t *Task) TimeoutOr(fallback time.Duration) time.Duration {
	if t.Timeout > 0 {
		return t.Timeout.D()
	}
	if fallback > 0 {
		return fallback
	}
	return defaultTimeout
}

// RepetitionsOr returns the task's repetition count, or the fallback.
func (t *Task) RepetitionsOr(fallback int) int {
	if t.Repetitions > 0 {
		return t.Repetitions
	}
	return max(fallback, 1)
}

// LoadTask reads and validates one task directory.
func LoadTask(dir string) (*Task, error) {
	spec := filepath.Join(dir, "task.json")
	raw, err := os.ReadFile(spec)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", spec, err)
	}

	// Absolute, because the checker runs with the workspace as its working
	// directory: a relative task path resolves against the wrong place and
	// fails to exec.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	var t Task
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	// An unknown field is almost always a typo in a hand-written spec, and a
	// silently ignored "max_iterations" that should have been "max_turns"
	// produces a suite that measures something other than what it claims to.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", spec, err)
	}
	t.Dir = abs
	if t.Check == "" {
		t.Check = defaultCheck
	}

	if err := t.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", spec, err)
	}
	return &t, nil
}

func (t *Task) validate() error {
	if t.Name == "" {
		return fmt.Errorf("name is required")
	}
	if base := filepath.Base(t.Dir); t.Name != base {
		return fmt.Errorf("name %q does not match directory %q; the report keys on "+
			"name and a mismatch makes two tasks impossible to tell apart", t.Name, base)
	}
	if strings.TrimSpace(t.Prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	if t.Category == "" {
		return fmt.Errorf("category is required")
	}

	info, err := os.Stat(t.RepoPath())
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s/ must exist and be a directory", repoDir)
	}

	check, err := os.Stat(t.CheckPath())
	if err != nil {
		return fmt.Errorf("checker %s: %w", t.Check, err)
	}
	if check.Mode().Perm()&0o111 == 0 {
		// Discovered at the end of a twenty-minute model run otherwise.
		return fmt.Errorf("checker %s is not executable (chmod +x)", t.Check)
	}

	if t.ExpectNoChanges && len(t.MustSurvive) > 0 {
		return fmt.Errorf("expect_no_changes already covers must_survive; listing both " +
			"suggests one of them is not saying what was meant")
	}
	return nil
}

// LoadSuite reads every task under root, in name order.
//
// Every task is validated before any is run. A typo in the twenty-seventh
// spec should cost a second, not the forty minutes of model calls it would
// take to reach it.
func LoadSuite(root string, only []string) ([]*Task, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("reading task root: %w", err)
	}

	wanted := map[string]bool{}
	for _, name := range only {
		if name = strings.TrimSpace(name); name != "" {
			wanted[name] = true
		}
	}

	// Filtering is by lookup only. An earlier version deleted each name as it
	// matched, so that the set emptied partway through the directory listing
	// and every task after the last match loaded as though no filter had been
	// given — `-only grep-discovery` quietly ran five tasks.
	var (
		tasks   []*Task
		bad     []string
		matched = map[string]bool{}
	)
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		if len(wanted) > 0 && !wanted[e.Name()] {
			continue
		}
		matched[e.Name()] = true

		t, err := LoadTask(filepath.Join(root, e.Name()))
		if err != nil {
			bad = append(bad, err.Error())
			continue
		}
		tasks = append(tasks, t)
	}

	for name := range wanted {
		if !matched[name] {
			// Silently running zero tasks looks exactly like a suite that passed.
			bad = append(bad, fmt.Sprintf("no such task: %s", name))
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return nil, fmt.Errorf("%d task(s) could not be loaded:\n  %s",
			len(bad), strings.Join(bad, "\n  "))
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("no tasks found under %s", root)
	}

	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Name < tasks[j].Name })
	return tasks, nil
}
