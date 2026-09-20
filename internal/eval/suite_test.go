package eval

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The shipped suite, checked without a model.
//
// A checker has two ways to be wrong and only one of them is visible from a
// suite run. Rejecting a correct solution shows up as a task that never
// passes — annoying, and obvious. *Accepting* the untouched repo reports a
// pass the agent did not earn, inflates every number that depends on it, and
// looks completely normal in the output.
//
// So the dangerous direction is asserted here, where it costs a second and
// runs on every commit. The other direction needs hand-written solutions and
// lives in evals/verify-checkers.sh, which also covers the near-misses each
// task is designed to reject.

const suiteRoot = "../../evals/tasks"

func TestShippedSuiteLoads(t *testing.T) {
	tasks, err := LoadSuite(suiteRoot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) < 10 {
		t.Errorf("found %d tasks; the suite is supposed to have at least 10", len(tasks))
	}

	// Every task explains itself. A task nobody can justify is one that will
	// be silently wrong for a year.
	for _, task := range tasks {
		if strings.TrimSpace(task.Notes) == "" {
			t.Errorf("%s has no notes saying what it probes", task.Name)
		}
	}
}

func TestShippedSuiteCategoriesAreDistinct(t *testing.T) {
	// The categories are the coverage argument. Two tasks sharing one is fine;
	// ten tasks in three categories means the suite tests less than it claims.
	tasks, err := LoadSuite(suiteRoot, nil)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]int{}
	for _, task := range tasks {
		seen[task.Category]++
	}
	if len(seen) < 8 {
		t.Errorf("%d tasks span only %d categories: %v", len(tasks), len(seen), seen)
	}
}

func TestNoCheckerPassesAnUntouchedRepo(t *testing.T) {
	tasks, err := LoadSuite(suiteRoot, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, task := range tasks {
		t.Run(task.Name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			workspace := filepath.Join(dir, "workspace")
			if err := CopyTree(task.RepoPath(), workspace); err != nil {
				t.Fatal(err)
			}
			// An agent that did nothing produced no answer and called no tools.
			artifacts := filepath.Join(dir, "artifacts")
			if err := writeArtifacts(artifacts, &Transcript{}); err != nil {
				t.Fatal(err)
			}

			absTask, err := filepath.Abs(task.Dir)
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(task.CheckPath())
			cmd.Dir = workspace
			cmd.Env = append(os.Environ(),
				"HARNESS_WORKSPACE="+workspace,
				"HARNESS_TASK_DIR="+absTask,
				"HARNESS_HIDDEN_DIR="+filepath.Join(absTask, hiddenDir),
				"HARNESS_ANSWER="+filepath.Join(artifacts, "answer.txt"),
				"HARNESS_EVENTS="+filepath.Join(artifacts, "events.jsonl"),
				"HARNESS_TASK="+task.Name,
			)

			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the checker passed a repo nobody touched, so this task "+
					"scores a free pass on every run:\n%s", out)
			}

			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == checkerBrokenExit {
				t.Skipf("checker cannot run here: %s", strings.TrimSpace(string(out)))
			}
		})
	}
}
