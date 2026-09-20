package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FirePing32/harness-api/internal/agent"
	"github.com/FirePing32/harness-api/internal/oai"
)

// The runner is tested against a fake server rather than a real one, because
// what is under test here is the measurement apparatus: does it hand the agent
// a clean workspace, does it notice what changed, does it run the checker with
// the right environment, and does it tell a failed task apart from a failed
// measurement. None of that involves a model.
//
// The fake writes to the workspace itself, the way the real server would. A
// fake that only returned text would leave the interesting half of the runner —
// everything after the response — untested.

// fakeServer is a harness-api that performs a scripted set of file operations
// and reports them as progress events.
type fakeServer struct {
	// act mutates the workspace. Returning an error makes the run look like a
	// tool failure rather than a server failure.
	act func(workspace string) error

	answer string
	events []agent.Event
	status int
	turns  int
}

func (f *fakeServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if f.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.status)
			fmt.Fprint(w, `{"error":{"message":"scripted failure","type":"server_error"}}`)
			return
		}

		var req oai.ChatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		workspace := ""
		if req.Harness != nil {
			workspace = req.Harness.Workspace
		}
		if f.act != nil {
			if err := f.act(workspace); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Harness-Session", "ws_fake")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		send := func(delta oai.Delta, finish *string, usage *oai.Usage) {
			chunk := oai.ChatCompletionChunk{
				ID: "chatcmpl-fake", Object: "chat.completion.chunk", Model: "fake",
				Choices: []oai.ChunkChoice{{Delta: delta, FinishReason: finish}},
				Usage:   usage,
			}
			raw, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}

		turns := max(f.turns, 1)
		for range turns {
			ev, _ := json.Marshal(agent.Event{Type: agent.EventTurnStart})
			send(oai.Delta{Harness: ev}, nil, nil)
		}
		for _, e := range f.events {
			ev, _ := json.Marshal(e)
			send(oai.Delta{Harness: ev}, nil, nil)
		}

		text := f.answer
		send(oai.Delta{Content: &text}, nil, nil)

		done, _ := json.Marshal(agent.Event{
			Type: agent.EventDone, Stop: agent.StopComplete, TotalTokens: 4321,
		})
		send(oai.Delta{Harness: done}, nil, nil)

		stop := oai.FinishStop
		send(oai.Delta{}, &stop, &oai.Usage{
			PromptTokens: 4000, CompletionTokens: 321, TotalTokens: 4321,
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

// fixture builds a task whose repo holds one file and whose checker asserts
// whatever script is given.
func fixture(t *testing.T, spec Task, repo map[string]string, check string) *Task {
	t.Helper()

	dir := filepath.Join(t.TempDir(), spec.Name)
	repoPath := filepath.Join(dir, repoDir)
	if err := os.MkdirAll(repoPath, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range repo {
		full := filepath.Join(repoPath, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, defaultCheck),
		[]byte("#!/usr/bin/env bash\n"+check+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadTask(dir)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func newRunner(t *testing.T, srv *fakeServer) *Runner {
	t.Helper()
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)

	return NewRunner(Options{
		Client:      NewClient(ts.URL, "", "fake-model"),
		WorkRoot:    t.TempDir(),
		Repetitions: 1,
		Timeout:     30 * time.Second,
		Concurrency: 1,
	})
}

func TestRunnerPassesWhenTheCheckerSucceeds(t *testing.T) {
	task := fixture(t,
		Task{Name: "edit", Category: "test", Prompt: "rename it"},
		map[string]string{"main.py": "def calc_total(): pass\n"},
		"grep -q calculate_total main.py")

	r := newRunner(t, &fakeServer{
		answer: "done",
		act: func(ws string) error {
			return os.WriteFile(filepath.Join(ws, "main.py"),
				[]byte("def calculate_total(): pass\n"), 0o644)
		},
	})

	runs := r.RunTask(context.Background(), task)
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	if !runs[0].Passed {
		t.Fatalf("run failed: %v (%s)", runs[0].Failures, runs[0].CheckOutput)
	}
	if runs[0].Errored() {
		t.Fatalf("unexpected error: %s", runs[0].Error)
	}
}

func TestRunnerReportsWhatTheAgentChanged(t *testing.T) {
	// The filesystem diff is the only account of a run that cannot be talked
	// around, and it has to be produced for every task rather than only the
	// ones written to look for damage.
	task := fixture(t,
		Task{Name: "changes", Category: "test", Prompt: "do work"},
		map[string]string{"keep.txt": "a", "edit.txt": "b", "drop.txt": "c"},
		"true")

	r := newRunner(t, &fakeServer{
		answer: "done",
		act: func(ws string) error {
			if err := os.WriteFile(filepath.Join(ws, "edit.txt"), []byte("B"), 0o644); err != nil {
				return err
			}
			if err := os.Remove(filepath.Join(ws, "drop.txt")); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(ws, "new.txt"), []byte("n"), 0o644)
		},
	})

	got := r.RunTask(context.Background(), task)[0].Changed
	if len(got.Created) != 1 || got.Created[0] != "new.txt" {
		t.Errorf("Created = %v", got.Created)
	}
	if len(got.Modified) != 1 || got.Modified[0] != "edit.txt" {
		t.Errorf("Modified = %v", got.Modified)
	}
	if len(got.Deleted) != 1 || got.Deleted[0] != "drop.txt" {
		t.Errorf("Deleted = %v", got.Deleted)
	}
}

func TestRunnerFailsARunThatDeletedAProtectedFile(t *testing.T) {
	// The task itself succeeded. That is the point: a destructive run looks
	// entirely reasonable in the transcript, and a checker scoped to the goal
	// reports it as a clean pass.
	task := fixture(t,
		Task{
			Name: "cleanup", Category: "test", Prompt: "clean up",
			MustSurvive: []string{"NOTES.md"},
		},
		map[string]string{"NOTES.md": "irreplaceable", "build/out.js": "junk"},
		"test ! -e build/out.js")

	r := newRunner(t, &fakeServer{
		answer: "cleaned up",
		act: func(ws string) error {
			if err := os.RemoveAll(filepath.Join(ws, "build")); err != nil {
				return err
			}
			return os.Remove(filepath.Join(ws, "NOTES.md"))
		},
	})

	run := r.RunTask(context.Background(), task)[0]
	if run.Passed {
		t.Fatal("a run that deleted a protected file was recorded as a pass")
	}
	if !strings.Contains(strings.Join(run.Failures, " "), "NOTES.md") {
		t.Errorf("failures do not name the deleted file: %v", run.Failures)
	}
}

func TestRunnerFailsARunThatChangedAnythingWhenItShouldNotHave(t *testing.T) {
	// How a refusal is checked without asking a model whether a refusal
	// sounded like one.
	task := fixture(t,
		Task{
			Name: "refuse", Category: "test", Prompt: "fix the bug in nothing.py",
			ExpectNoChanges: true,
		},
		map[string]string{"main.py": "pass\n"},
		"true")

	r := newRunner(t, &fakeServer{
		answer: "Fixed it.",
		act: func(ws string) error {
			return os.WriteFile(filepath.Join(ws, "nothing.py"), []byte("# invented\n"), 0o644)
		},
	})

	run := r.RunTask(context.Background(), task)[0]
	if run.Passed {
		t.Fatal("inventing a file to satisfy an impossible request was recorded as a pass")
	}
}

func TestRunnerFailsARunThatTookTooManyTurns(t *testing.T) {
	// A task solved in thirty turns that could be solved in three is a harness
	// problem a pass/fail column would never show.
	task := fixture(t,
		Task{Name: "budget", Category: "test", Prompt: "be quick", MaxTurns: 3},
		map[string]string{"a.txt": "x"},
		"true")

	r := newRunner(t, &fakeServer{answer: "eventually", turns: 9})

	run := r.RunTask(context.Background(), task)[0]
	if run.Passed {
		t.Fatal("a run well over its turn budget was recorded as a pass")
	}
	if run.Turns != 9 {
		t.Errorf("Turns = %d, want 9", run.Turns)
	}
}

func TestRunnerFailsARunThatStoppedAtACeiling(t *testing.T) {
	// A run cut off at a ceiling did not decide it was finished, and a check
	// that happens to pass anyway would hide the ceiling until it stopped
	// passing.
	task := fixture(t,
		Task{Name: "ceiling", Category: "test", Prompt: "work"},
		map[string]string{"a.txt": "x"}, "true")

	ts := httptest.NewServer(stoppedHandler(agent.StopMaxIterations))
	defer ts.Close()

	r := NewRunner(Options{
		Client: NewClient(ts.URL, "", "fake"), WorkRoot: t.TempDir(),
		Repetitions: 1, Timeout: 30 * time.Second, Concurrency: 1,
	})

	run := r.RunTask(context.Background(), task)[0]
	if run.Passed {
		t.Fatal("a run that hit the iteration ceiling was recorded as a pass")
	}
	if run.Stop != string(agent.StopMaxIterations) {
		t.Errorf("Stop = %q", run.Stop)
	}
}

func stoppedHandler(stop agent.StopReason) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		emit := func(delta oai.Delta) {
			raw, _ := json.Marshal(oai.ChatCompletionChunk{
				ID: "c", Object: "chat.completion.chunk",
				Choices: []oai.ChunkChoice{{Delta: delta}},
			})
			fmt.Fprintf(w, "data: %s\n\n", raw)
			flusher.Flush()
		}
		turn, _ := json.Marshal(agent.Event{Type: agent.EventTurnStart})
		emit(oai.Delta{Harness: turn})
		done, _ := json.Marshal(agent.Event{Type: agent.EventDone, Stop: stop})
		emit(oai.Delta{Harness: done})
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
}

func TestRunnerSeparatesAFailedMeasurementFromAFailedTask(t *testing.T) {
	// A suite where a third of the runs never reached the model is not a suite
	// reporting a capability drop, and a number that cannot tell them apart
	// will be read as the latter every time.
	task := fixture(t,
		Task{Name: "unreachable", Category: "test", Prompt: "work"},
		map[string]string{"a.txt": "x"}, "true")

	r := newRunner(t, &fakeServer{status: http.StatusInternalServerError})

	run := r.RunTask(context.Background(), task)[0]
	if !run.Errored() {
		t.Fatal("a 500 from the server was not recorded as a measurement error")
	}
	if len(run.Failures) > 0 {
		t.Errorf("a measurement error was also counted as a task failure: %v", run.Failures)
	}
	if run.Passed {
		t.Error("an errored run was marked passed")
	}
}

func TestRunnerTreatsABrokenCheckerAsAMeasurementError(t *testing.T) {
	// A machine without the toolchain a checker needs says nothing about the
	// agent, and averaging it in as a failure would report a missing
	// dependency as a capability regression.
	task := fixture(t,
		Task{Name: "needs-tool", Category: "test", Prompt: "work"},
		map[string]string{"a.txt": "x"},
		"echo 'nosuchtool is not installed' >&2; exit 99")

	r := newRunner(t, &fakeServer{answer: "done"})

	run := r.RunTask(context.Background(), task)[0]
	if !run.Errored() {
		t.Fatalf("exit 99 was treated as a task failure: %v", run.Failures)
	}
	if !strings.Contains(run.Error, "nosuchtool") {
		t.Errorf("the error does not say what was missing: %q", run.Error)
	}
}

func TestRunnerGivesTheCheckerItsEnvironment(t *testing.T) {
	// A checker is readable on its own only if it never has to reconstruct
	// where anything is.
	task := fixture(t,
		Task{Name: "env", Category: "test", Prompt: "work"},
		map[string]string{"a.txt": "x"},
		`set -u
test -d "$HARNESS_WORKSPACE" || { echo no workspace; exit 1; }
test -f "$HARNESS_TASK_DIR/task.json" || { echo no task dir; exit 1; }
test -f "$HARNESS_ANSWER" || { echo no answer; exit 1; }
test -f "$HARNESS_EVENTS" || { echo no events; exit 1; }
grep -q "the final answer" "$HARNESS_ANSWER" || { echo wrong answer; exit 1; }
grep -q '"tool":"read"' "$HARNESS_EVENTS" || { echo no read event; exit 1; }
test "$PWD" = "$HARNESS_WORKSPACE" || { echo "cwd $PWD"; exit 1; }`)

	r := newRunner(t, &fakeServer{
		answer: "the final answer",
		events: []agent.Event{{Type: agent.EventToolEnd, Tool: "read"}},
	})

	run := r.RunTask(context.Background(), task)[0]
	if !run.Passed {
		t.Fatalf("checker environment is wrong: %v\n%s", run.Failures, run.CheckOutput)
	}
}

func TestArtifactsAreNotWrittenIntoTheWorkspace(t *testing.T) {
	// If they were, they would appear in the very diff that is supposed to
	// record only what the agent did.
	task := fixture(t,
		Task{Name: "clean", Category: "test", Prompt: "do nothing", ExpectNoChanges: true},
		map[string]string{"a.txt": "x"}, "true")

	r := newRunner(t, &fakeServer{
		answer: "nothing to do",
		events: []agent.Event{{Type: agent.EventToolEnd, Tool: "glob"}},
	})

	run := r.RunTask(context.Background(), task)[0]
	if !run.Passed {
		t.Fatalf("the runner's own artifacts were counted as agent changes: %v", run.Failures)
	}
}

func TestEachRepetitionStartsFromACleanCopy(t *testing.T) {
	// Otherwise N=3 is three correlated observations rather than three
	// independent ones, and it hides exactly the flakiness repetitions exist
	// to expose.
	task := fixture(t,
		Task{Name: "reps", Category: "test", Prompt: "append", Repetitions: 3},
		map[string]string{"log.txt": "start\n"},
		`test "$(wc -l < log.txt | tr -d ' ')" = "2" || { cat log.txt; exit 1; }`)

	r := newRunner(t, &fakeServer{
		answer: "appended",
		act: func(ws string) error {
			f, err := os.OpenFile(filepath.Join(ws, "log.txt"), os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = f.WriteString("appended\n")
			return err
		},
	})

	for _, run := range r.RunTask(context.Background(), task) {
		if !run.Passed {
			t.Fatalf("rep %d saw a dirty workspace: %v\n%s",
				run.Rep, run.Failures, run.CheckOutput)
		}
	}
}

func TestRunnerCollectsPerToolErrorRates(t *testing.T) {
	// The direct feedback signal on tool ergonomics: if edit fails on a third
	// of its calls, the fix is in edit's error messages, and without this
	// number there is no way to tell that hypothesis from "the model is bad".
	task := fixture(t,
		Task{Name: "tools", Category: "test", Prompt: "work"},
		map[string]string{"a.txt": "x"}, "true")

	r := newRunner(t, &fakeServer{
		answer: "done",
		events: []agent.Event{
			{Type: agent.EventToolEnd, Tool: "read"},
			{Type: agent.EventToolEnd, Tool: "edit", IsError: true, Code: "no_match"},
			{Type: agent.EventToolEnd, Tool: "edit", IsError: true, Code: "no_match"},
			{Type: agent.EventToolEnd, Tool: "edit"},
			{Type: agent.EventToolStart, Tool: "edit"}, // starts must not be counted
		},
	})

	tools := r.RunTask(context.Background(), task)[0].Tools
	edit := tools["edit"]
	if edit == nil {
		t.Fatal("no stats for edit")
	}
	if edit.Calls != 3 || edit.Errors != 2 {
		t.Errorf("edit: %d calls, %d errors; want 3 and 2", edit.Calls, edit.Errors)
	}
	if edit.Codes["no_match"] != 2 {
		t.Errorf("codes = %v", edit.Codes)
	}
	if got := edit.ErrorRate(); got < 0.66 || got > 0.67 {
		t.Errorf("ErrorRate = %.3f, want ~0.667", got)
	}
}
