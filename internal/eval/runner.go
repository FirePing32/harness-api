package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FirePing32/harness-api/internal/oai"
)

// Running one repetition, and the difference between "the agent failed" and
// "the measurement failed".
//
// Those two are recorded separately and never merged. A suite where a third of
// the runs never reached the model is not a suite reporting a 33% capability
// drop, and a number that cannot tell them apart will be read as the latter
// every time.

const (
	// checkOutputLimit is how much of a checker's output is kept. Enough for a
	// go test failure, short of a full build log.
	checkOutputLimit = 16 << 10

	// answerLimit bounds what is written to the artifact file.
	answerLimit = 256 << 10

	// checkerBrokenExit is a checker saying it could not run — a missing
	// toolchain, an absent fixture. It is reported as a measurement error, not
	// a failed task, because a machine without Go installed is not evidence
	// about the agent and must not be averaged in as though it were.
	checkerBrokenExit = 99
)

// Options configures a Runner.
type Options struct {
	Client *Client

	// WorkRoot is where per-repetition workspaces are created.
	WorkRoot string

	// Repetitions is the default when a task does not set its own.
	Repetitions int

	// Timeout is the default per-repetition ceiling.
	Timeout time.Duration

	// Concurrency is how many repetitions run at once. Agent runs are mostly
	// waiting on a model, so some parallelism is close to free — but it is not
	// free to a rate-limited account, which is why it is a knob and not a
	// constant.
	Concurrency int

	// Keep retains workspaces after a run. Off by default because thirty tasks
	// times three repetitions is ninety trees; on, it is the only way to see
	// what the agent actually did to a failing one.
	Keep bool

	// Progress is called as each repetition finishes.
	Progress func(Run)
}

// Runner executes tasks against a server.
type Runner struct {
	opts Options
}

// NewRunner builds a Runner.
func NewRunner(opts Options) *Runner {
	if opts.Repetitions <= 0 {
		opts.Repetitions = 3
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 1
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	return &Runner{opts: opts}
}

// Run is one repetition of one task.
type Run struct {
	Task string `json:"task"`
	Rep  int    `json:"rep"`

	Passed bool `json:"passed"`

	// Failures names every check that did not hold, not merely the first. A
	// run that both missed the goal and deleted a file should say so: fixing
	// the first and rediscovering the second is two debugging sessions.
	Failures []string `json:"failures,omitempty"`

	// Error is set when the run could not be measured at all — the server was
	// unreachable, the request was rejected, the checker would not start. It
	// is deliberately not a Failure: this run says nothing about the agent.
	Error string `json:"error,omitempty"`

	Turns      int       `json:"turns"`
	Tools      ToolStats `json:"tools,omitempty"`
	Usage      oai.Usage `json:"usage"`
	Stop       string    `json:"stop,omitempty"`
	DurationMS int64     `json:"duration_ms"`

	Changed Change `json:"changed,omitempty"`

	// CheckOutput is the tail of the checker's combined output, kept only for
	// failures. On a pass it is noise; on a failure it is usually the answer.
	CheckOutput string `json:"check_output,omitempty"`

	// Workspace is where the run happened, set only when workspaces are kept.
	Workspace string `json:"workspace,omitempty"`
}

// Errored reports whether the run failed to produce a measurement.
func (r Run) Errored() bool { return r.Error != "" }

// RunTask executes every repetition of a task.
func (r *Runner) RunTask(ctx context.Context, t *Task) []Run {
	reps := t.RepetitionsOr(r.opts.Repetitions)
	runs := make([]Run, reps)

	sem := make(chan struct{}, r.opts.Concurrency)
	var wg sync.WaitGroup
	for i := range reps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			if ctx.Err() != nil {
				runs[i] = Run{Task: t.Name, Rep: i + 1, Error: ctx.Err().Error()}
				return
			}
			runs[i] = r.runOnce(ctx, t, i+1)
			if r.opts.Progress != nil {
				r.opts.Progress(runs[i])
			}
		}(i)
	}
	wg.Wait()
	return runs
}

func (r *Runner) runOnce(ctx context.Context, t *Task, rep int) Run {
	run := Run{Task: t.Name, Rep: rep}
	start := time.Now()
	defer func() { run.DurationMS = time.Since(start).Milliseconds() }()

	dir, cleanup, err := r.prepare(t, rep)
	if err != nil {
		run.Error = err.Error()
		return run
	}
	defer cleanup()

	workspace := filepath.Join(dir, "workspace")
	if r.opts.Keep {
		run.Workspace = workspace
	}

	before, err := Scan(workspace)
	if err != nil {
		run.Error = fmt.Sprintf("snapshotting the workspace: %v", err)
		return run
	}

	runCtx, cancel := context.WithTimeout(ctx, t.TimeoutOr(r.opts.Timeout))
	defer cancel()

	transcript, err := r.opts.Client.Run(runCtx, Request{
		Prompt:    t.Prompt,
		Workspace: workspace,
		Tools:     t.Tools,
	})
	if err != nil {
		// A timeout is the agent failing to finish, which is a real result.
		// Anything else means the measurement did not happen.
		if runCtx.Err() != nil && ctx.Err() == nil {
			run.Failures = append(run.Failures,
				fmt.Sprintf("timed out after %s", t.TimeoutOr(r.opts.Timeout)))
			run.Stop = "timeout"
			return run
		}
		run.Error = err.Error()
		return run
	}

	run.Turns = transcript.Turns
	run.Tools = transcript.Tools()
	run.Usage = transcript.Usage
	run.Stop = string(transcript.Stop)

	after, err := Scan(workspace)
	if err != nil {
		run.Error = fmt.Sprintf("snapshotting the workspace: %v", err)
		return run
	}
	run.Changed = Diff(before, after)

	// Artifacts live outside the workspace. Writing them inside would be more
	// convenient for checkers and would show up in the very diff that is
	// supposed to record only what the agent did.
	artifacts := filepath.Join(dir, "artifacts")
	if err := writeArtifacts(artifacts, transcript); err != nil {
		run.Error = fmt.Sprintf("writing artifacts: %v", err)
		return run
	}

	run.Failures = append(run.Failures, declarativeFailures(t, run.Changed, transcript)...)

	output, checkErr, fatal := r.check(ctx, t, workspace, artifacts)
	if fatal != nil {
		run.Error = fatal.Error()
		return run
	}
	if checkErr != "" {
		run.Failures = append(run.Failures, checkErr)
	}
	if len(run.Failures) > 0 {
		run.CheckOutput = tail(output, checkOutputLimit)
	}

	run.Passed = len(run.Failures) == 0
	return run
}

// prepare builds a fresh workspace for one repetition.
func (r *Runner) prepare(t *Task, rep int) (string, func(), error) {
	root := r.opts.WorkRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}

	dir, err := os.MkdirTemp(root, fmt.Sprintf("eval-%s-%d-", t.Name, rep))
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		if !r.opts.Keep {
			os.RemoveAll(dir)
		}
	}

	if err := CopyTree(t.RepoPath(), filepath.Join(dir, "workspace")); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("copying the task repo: %w", err)
	}

	// Resolve once, here, so every consumer sees the same string. On macOS
	// TempDir is under /var, which is a symlink to /private/var: a checker
	// comparing $PWD against $HARNESS_WORKSPACE would find two spellings of
	// one directory and conclude it was in the wrong place.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	return resolved, cleanup, nil
}

// declarativeFailures applies the checks the spec expresses directly.
//
// These run even when the checker passes. "The task succeeded and the agent
// deleted the README" is a result a pass/fail column would report as a clean
// success, and it is the most important thing that run has to say.
func declarativeFailures(t *Task, changed Change, tr *Transcript) []string {
	var out []string

	if t.ExpectNoChanges && !changed.Empty() {
		out = append(out, fmt.Sprintf(
			"expected no changes, but %d path(s) changed: %s",
			changed.Total(), summariseChange(changed)))
	}

	for _, path := range t.MustSurvive {
		for _, gone := range changed.Deleted {
			if gone == path {
				out = append(out, fmt.Sprintf("deleted a file that had to survive: %s", path))
			}
		}
	}

	if t.MaxTurns > 0 && tr.Turns > t.MaxTurns {
		out = append(out, fmt.Sprintf("took %d turns, budget was %d", tr.Turns, t.MaxTurns))
	}

	// A run that stopped at a ceiling did not decide it was finished; it was
	// cut off. The check may still pass by luck, and calling that a success
	// would hide the ceiling until it started failing.
	if tr.Stop != "" && tr.Stop != "complete" {
		out = append(out, fmt.Sprintf("stopped at a ceiling: %s", tr.Stop))
	}

	return out
}

func summariseChange(c Change) string {
	var parts []string
	for label, paths := range map[string][]string{
		"created": c.Created, "modified": c.Modified, "deleted": c.Deleted,
	} {
		if len(paths) > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", label, strings.Join(clip(paths, 5), ", ")))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// check runs the task's checker in the workspace.
//
// The checker gets the workspace as its working directory and everything else
// through the environment, so a checker is readable on its own: it never has
// to reconstruct where anything is.
func (r *Runner) check(
	ctx context.Context, t *Task, workspace, artifacts string,
) (output, failure string, fatal error) {
	checkCtx, cancel := context.WithTimeout(ctx, t.TimeoutOr(r.opts.Timeout))
	defer cancel()

	cmd := exec.CommandContext(checkCtx, t.CheckPath())
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(),
		"HARNESS_WORKSPACE="+workspace,
		"HARNESS_TASK_DIR="+t.Dir,
		"HARNESS_HIDDEN_DIR="+filepath.Join(t.Dir, hiddenDir),
		"HARNESS_ANSWER="+filepath.Join(artifacts, "answer.txt"),
		"HARNESS_EVENTS="+filepath.Join(artifacts, "events.jsonl"),
		"HARNESS_TASK="+t.Name,
	)

	raw, err := cmd.CombinedOutput()
	output = string(raw)

	switch {
	case err == nil:
		return output, "", nil
	case checkCtx.Err() != nil:
		return output, "the checker timed out", nil
	default:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if exit.ExitCode() == checkerBrokenExit {
				return output, "", fmt.Errorf("the checker could not run: %s",
					strings.TrimSpace(tail(output, 512)))
			}
			return output, fmt.Sprintf("check failed (exit %d)", exit.ExitCode()), nil
		}
		// Could not start: a missing interpreter, a bad shebang. That is a
		// broken measurement, not a failed agent.
		return output, "", fmt.Errorf("running the checker: %w", err)
	}
}

// writeArtifacts records what the agent said and did, for the checker and for
// anyone reading a failure afterwards.
func writeArtifacts(dir string, t *Transcript) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	answer := t.Answer
	if len(answer) > answerLimit {
		answer = answer[:answerLimit]
	}
	if err := os.WriteFile(filepath.Join(dir, "answer.txt"), []byte(answer), 0o600); err != nil {
		return err
	}

	var events strings.Builder
	for _, ev := range t.Events {
		line, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		events.Write(line)
		events.WriteByte('\n')
	}
	return os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(events.String()), 0o600)
}

// tail keeps the end of a checker's output.
//
// The same asymmetry as shell output, for the same reason: a failing build
// puts its error at the bottom, under the progress log.
func tail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := s[len(s)-limit:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < 200 {
		cut = cut[i+1:]
	}
	return "… output truncated …\n" + cut
}

func clip(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := append([]string(nil), items[:n]...)
	return append(out, "and "+strconv.Itoa(len(items)-n)+" more")
}
