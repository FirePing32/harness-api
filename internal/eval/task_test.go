package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTask lays out a task directory from raw JSON, so the tests can express
// malformed specs that the Task struct could not.
func writeTask(t *testing.T, root, name, spec string, executable bool) string {
	t.Helper()

	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, repoDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := os.WriteFile(filepath.Join(dir, defaultCheck),
		[]byte("#!/usr/bin/env bash\ntrue\n"), mode); err != nil {
		t.Fatal(err)
	}
	return dir
}

const validSpec = `{"name":"%s","category":"test","prompt":"do the thing"}`

func TestLoadTaskAcceptsAValidSpec(t *testing.T) {
	dir := writeTask(t, t.TempDir(), "good", `{
		"name":"good","category":"test","prompt":"do it",
		"timeout":"90s","repetitions":5,"max_turns":12
	}`, true)

	got, err := LoadTask(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutOr(time.Hour) != 90*time.Second {
		t.Errorf("Timeout = %s", got.TimeoutOr(time.Hour))
	}
	if got.RepetitionsOr(3) != 5 {
		t.Errorf("Repetitions = %d", got.RepetitionsOr(3))
	}
	if got.Check != defaultCheck {
		t.Errorf("Check = %q, want the default", got.Check)
	}
}

func TestLoadTaskRejectsAnUnknownField(t *testing.T) {
	// A silently ignored "max_iterations" where "max_turns" was meant produces
	// a suite that measures something other than what it claims to, and
	// nothing about the output looks wrong.
	dir := writeTask(t, t.TempDir(), "typo",
		`{"name":"typo","category":"test","prompt":"x","max_iterations":5}`, true)

	_, err := LoadTask(dir)
	if err == nil {
		t.Fatal("a typo in a spec field was accepted")
	}
	if !strings.Contains(err.Error(), "max_iterations") {
		t.Errorf("the error does not name the offending field: %v", err)
	}
}

func TestLoadTaskRejectsANameThatDoesNotMatchItsDirectory(t *testing.T) {
	// The report keys on name; a mismatch makes two tasks impossible to tell
	// apart afterwards.
	dir := writeTask(t, t.TempDir(), "on-disk",
		`{"name":"in-spec","category":"test","prompt":"x"}`, true)

	if _, err := LoadTask(dir); err == nil {
		t.Fatal("a name/directory mismatch was accepted")
	}
}

func TestLoadTaskRejectsANonExecutableChecker(t *testing.T) {
	// Otherwise discovered at the end of a twenty-minute model run.
	dir := writeTask(t, t.TempDir(), "unrunnable",
		`{"name":"unrunnable","category":"test","prompt":"x"}`, false)

	_, err := LoadTask(dir)
	if err == nil {
		t.Fatal("a non-executable checker was accepted")
	}
	if !strings.Contains(err.Error(), "chmod") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func TestLoadTaskRejectsContradictoryExpectations(t *testing.T) {
	// expect_no_changes already implies must_survive. Asking for both means
	// one of them is not saying what was meant.
	dir := writeTask(t, t.TempDir(), "muddled", `{
		"name":"muddled","category":"test","prompt":"x",
		"expect_no_changes":true,"must_survive":["a.txt"]
	}`, true)

	if _, err := LoadTask(dir); err == nil {
		t.Fatal("a spec asking for both was accepted")
	}
}

func TestLoadSuiteValidatesEveryTaskBeforeRunningAny(t *testing.T) {
	// A typo in the twenty-seventh spec should cost a second, not the forty
	// minutes of model calls it takes to reach it. So the error names every
	// broken task, not the first.
	root := t.TempDir()
	writeTask(t, root, "fine", `{"name":"fine","category":"test","prompt":"x"}`, true)
	writeTask(t, root, "broken-a", `{"name":"broken-a","category":"test"}`, true)
	writeTask(t, root, "broken-b", `{"name":"wrong","category":"test","prompt":"x"}`, true)

	_, err := LoadSuite(root, nil)
	if err == nil {
		t.Fatal("a suite with two broken specs loaded cleanly")
	}
	for _, want := range []string{"broken-a", "broken-b"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadSuiteRunsOnlyWhatWasAskedFor(t *testing.T) {
	// Found by running the real binary: the filter used to delete each name as
	// it matched, so the set emptied partway through the sorted listing and
	// every task after the last match loaded as though no filter had been
	// given. `-only c` ran c, d and e.
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		writeTask(t, root, name,
			`{"name":"`+name+`","category":"test","prompt":"x"}`, true)
	}

	tasks, err := LoadSuite(root, []string{"c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Name != "c" {
		names := make([]string, len(tasks))
		for i, task := range tasks {
			names[i] = task.Name
		}
		t.Fatalf("loaded %v, want only [c]", names)
	}

	// And a filter naming several still gets all of them.
	tasks, err = LoadSuite(root, []string{"a", "e"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Errorf("loaded %d tasks for a two-name filter", len(tasks))
	}
}

func TestTaskDirIsAbsolute(t *testing.T) {
	// The checker runs with the workspace as its working directory, so a
	// relative task path resolves against the wrong place and fails to exec.
	root := t.TempDir()
	writeTask(t, root, "rel", `{"name":"rel","category":"test","prompt":"x"}`, true)

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(wd, filepath.Join(root, "rel"))
	if err != nil {
		t.Skip("no relative path between the test dir and the temp dir")
	}

	task, err := LoadTask(relative)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(task.Dir) {
		t.Errorf("Dir = %q, want an absolute path", task.Dir)
	}
	if !filepath.IsAbs(task.CheckPath()) {
		t.Errorf("CheckPath = %q, want an absolute path", task.CheckPath())
	}
}

func TestLoadSuiteReportsAFilterThatMatchesNothing(t *testing.T) {
	// Silently running zero tasks looks exactly like a suite that passed.
	root := t.TempDir()
	writeTask(t, root, "real", `{"name":"real","category":"test","prompt":"x"}`, true)

	_, err := LoadSuite(root, []string{"real", "imaginary"})
	if err == nil {
		t.Fatal("a misspelled -only name was ignored")
	}
	if !strings.Contains(err.Error(), "imaginary") {
		t.Errorf("the error does not name the missing task: %v", err)
	}
}

func TestLoadSuiteSkipsUnderscoredEntries(t *testing.T) {
	// _lib.sh and friends live alongside the tasks.
	root := t.TempDir()
	writeTask(t, root, "real", `{"name":"real","category":"test","prompt":"x"}`, true)
	if err := os.WriteFile(filepath.Join(root, "_lib.sh"), []byte("# helpers\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	tasks, err := LoadSuite(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Errorf("loaded %d tasks, want 1", len(tasks))
	}
}

func TestDurationRoundTripsAsAReadableString(t *testing.T) {
	// A spec is read by people; a count of nanoseconds cannot be checked by eye.
	var d Duration
	if err := json.Unmarshal([]byte(`"2m30s"`), &d); err != nil {
		t.Fatal(err)
	}
	if d.D() != 150*time.Second {
		t.Errorf("parsed %s", d.D())
	}

	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"2m30s"` {
		t.Errorf("marshalled as %s", raw)
	}

	if err := json.Unmarshal([]byte(`90`), &d); err == nil {
		t.Error("a bare number was accepted as a duration")
	}
}
