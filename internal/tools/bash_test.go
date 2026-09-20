package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/workspace"
)

func requireBash(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no bash on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
}

func bashTool(mutate ...func(*config.Shell)) *Bash {
	cfg := config.Default().Shell
	cfg.DefaultTimeout = config.Duration(10 * time.Second)
	cfg.MaxTimeout = config.Duration(30 * time.Second)
	for _, m := range mutate {
		m(&cfg)
	}
	return NewBash(workspace.NewLocalShell(), cfg)
}

func TestBashRunsACommand(t *testing.T) {
	requireBash(t)
	s := newSession(t, nil)

	text, out, err := run(t, bashTool(), s, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "hello") {
		t.Errorf("output = %q", text)
	}
	if r := out.(*BashResult); r.ExitCode != 0 {
		t.Errorf("ExitCode = %d", r.ExitCode)
	}
}

func TestBashNonZeroExitIsNotAToolError(t *testing.T) {
	// A failing test suite is information the model needs. Turning it into a
	// tool failure would hide the output that explains it.
	requireBash(t)
	s := newSession(t, nil)

	text, out, err := run(t, bashTool(), s,
		map[string]any{"command": "echo 'tests failed'; exit 3"})
	if err != nil {
		t.Fatalf("a non-zero exit was reported as a tool error: %v", err)
	}
	r := out.(*BashResult)
	if r.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", r.ExitCode)
	}
	if !strings.Contains(text, "tests failed") {
		t.Errorf("the output was lost: %q", text)
	}
	if !strings.Contains(text, "Exit code: 3") {
		t.Errorf("the exit code is not reported to the model: %q", text)
	}
}

func TestBashMergesStderrIntoStdoutInOrder(t *testing.T) {
	// A build that prints progress to stdout and its error to stderr is only
	// readable if the two arrive in the order they were written.
	requireBash(t)
	s := newSession(t, nil)

	text, _, err := run(t, bashTool(), s, map[string]any{
		"command": "echo first; echo second >&2; echo third",
	})
	if err != nil {
		t.Fatal(err)
	}
	iFirst := strings.Index(text, "first")
	iSecond := strings.Index(text, "second")
	iThird := strings.Index(text, "third")
	if iFirst < 0 || iSecond < 0 || iThird < 0 {
		t.Fatalf("a stream was dropped: %q", text)
	}
	if !(iFirst < iSecond && iSecond < iThird) {
		t.Errorf("streams are out of order: %q", text)
	}
}

func TestBashHonoursWorkdirWithoutCd(t *testing.T) {
	requireBash(t)
	s := newSession(t, map[string]string{"sub/marker.txt": "x\n"})

	text, _, err := run(t, bashTool(), s, map[string]any{
		"command": "ls", "workdir": "sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "marker.txt") {
		t.Errorf("the command did not run in the requested directory: %q", text)
	}
}

func TestBashIsStatelessBetweenCalls(t *testing.T) {
	// The documented contract, and the reason the tool description tells the
	// model to use workdir and && rather than expecting state to persist.
	requireBash(t)
	s := newSession(t, map[string]string{"sub/marker.txt": "x\n"})
	tool := bashTool()

	if _, _, err := run(t, tool, s, map[string]any{
		"command": "cd sub && export MARKER=set",
	}); err != nil {
		t.Fatal(err)
	}

	text, _, err := run(t, tool, s, map[string]any{"command": "pwd; echo \"MARKER=[$MARKER]\""})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "/sub") {
		t.Errorf("the working directory carried over between calls: %q", text)
	}
	if !strings.Contains(text, "MARKER=[]") {
		t.Errorf("an exported variable carried over between calls: %q", text)
	}
}

func TestBashWorkdirMustBeInsideTheWorkspace(t *testing.T) {
	requireBash(t)
	s := newSession(t, nil)

	_, _, err := run(t, bashTool(), s, map[string]any{
		"command": "ls", "workdir": "../..",
	})
	var pe *workspace.PathError
	if !errorAs(err, &pe) {
		t.Fatalf("err = %v, want a workspace.PathError", err)
	}
}

func TestBashMissingWorkdirIsNamed(t *testing.T) {
	requireBash(t)
	s := newSession(t, nil)

	_, _, err := run(t, bashTool(), s, map[string]any{
		"command": "ls", "workdir": "nope",
	})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeNotFound {
		t.Fatalf("err = %v, want NOT_FOUND", err)
	}
}

func TestBashTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	// The case that matters. `npm test` is a shell that spawns node, which
	// spawns workers. Killing only the process bash forked leaves every one of
	// them running, holding ports long after the request returned.
	requireBash(t)
	s := newSession(t, nil)

	marker := filepath.Join(t.TempDir(), "child-still-alive")
	// The child outlives its parent shell unless the whole group is signalled.
	cmd := "bash -c 'sleep 1; echo alive > " + marker + "' & sleep 30"

	start := time.Now()
	text, out, err := run(t, bashTool(func(c *config.Shell) {
		c.DefaultTimeout = config.Duration(300 * time.Millisecond)
	}), s, map[string]any{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	r := out.(*BashResult)
	if !r.TimedOut {
		t.Error("TimedOut is false after the command was killed")
	}
	if elapsed > 10*time.Second {
		t.Errorf("the timeout took %s to take effect", elapsed)
	}
	// A timeout is not a failure, and saying only "exit 137" makes the model
	// rewrite a command that was working.
	if !strings.Contains(text, "did not finish") {
		t.Errorf("the timeout is not explained as a timeout: %q", text)
	}
	if !strings.Contains(text, "timeout_seconds") {
		t.Errorf("the message does not say how to give it longer: %q", text)
	}

	// Give the orphan long enough to write its marker if it survived.
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a grandchild process survived the timeout; the process group was not killed")
	}
}

func TestBashCapsOutputKeepingTheEnd(t *testing.T) {
	// The opposite of a file view, deliberately: the error at the end of a
	// failed build is under however many kilobytes of progress.
	requireBash(t)
	s := newSession(t, nil)

	text, out, err := run(t, bashTool(func(c *config.Shell) {
		c.TailBytes = 2 << 10
	}), s, map[string]any{
		"command": "for i in $(seq 1 3000); do echo \"progress line $i\"; done; " +
			"echo 'FATAL: the real error'",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*BashResult)

	if !r.Truncated {
		t.Fatal("large output was not marked truncated")
	}
	if !strings.Contains(text, "FATAL: the real error") {
		t.Error("truncation discarded the end of the output, which is where errors are")
	}
	if strings.Contains(text, "progress line 1\n") {
		t.Error("the beginning survived; truncation should drop the head")
	}
	if len(r.Output) > 4<<10 {
		t.Errorf("kept %d bytes, well over the cap", len(r.Output))
	}
}

func TestBashSpillsFullOutputToAReadableFile(t *testing.T) {
	requireBash(t)
	s := newSession(t, nil)

	_, out, err := run(t, bashTool(func(c *config.Shell) {
		c.TailBytes = 1 << 10
	}), s, map[string]any{
		"command": "for i in $(seq 1 2000); do echo \"line $i\"; done",
	})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*BashResult)

	if r.SpillPath == "" {
		t.Fatal("no overflow file was written for truncated output")
	}
	// The model has to be able to reach it with the ordinary tools.
	content, err := s.Jail().ReadFile(r.SpillPath)
	if err != nil {
		t.Fatalf("the overflow file is not readable: %v", err)
	}
	if !strings.Contains(string(content), "line 1\n") {
		t.Error("the overflow file is missing the beginning of the output")
	}
	if !strings.Contains(string(content), "line 2000") {
		t.Error("the overflow file is missing the end of the output")
	}
}

func TestBashSpillIsNotCreatedForOrdinaryOutput(t *testing.T) {
	// Almost every command prints a few lines. Creating and deleting a file for
	// each would leave a trail of churn in somebody's working tree.
	requireBash(t)
	s := newSession(t, nil)

	_, out, err := run(t, bashTool(), s, map[string]any{"command": "echo small"})
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*BashResult); r.SpillPath != "" {
		t.Errorf("an overflow file was written for tiny output: %q", r.SpillPath)
	}
	if _, err := s.Jail().Stat(".harness"); err == nil {
		t.Error("the overflow directory was created even though nothing overflowed")
	}
}

func TestBashSpillDirectoryIsHiddenFromSearches(t *testing.T) {
	// Otherwise a later workspace-wide grep matches the output of a command the
	// agent ran minutes ago, which reads as a real result and is not one.
	requireBash(t)
	s := newSession(t, nil)

	if _, _, err := run(t, bashTool(func(c *config.Shell) {
		c.TailBytes = 512
	}), s, map[string]any{
		"command": "for i in $(seq 1 500); do echo 'DISTINCTIVE_TOKEN'; done",
	}); err != nil {
		t.Fatal(err)
	}
	s.ReloadIgnore()

	_, out, err := run(t, NewGrep(), s, map[string]any{"pattern": "DISTINCTIVE_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*GrepResult); r.Total != 0 {
		t.Errorf("grep matched %d lines inside the overflow directory", r.Total)
	}

	_, globOut, err := run(t, NewGlob(), s, map[string]any{"pattern": "**/*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range globPaths(t, globOut) {
		if strings.HasPrefix(p, ".harness") {
			t.Errorf("glob listed the overflow directory: %q", p)
		}
	}
}

func TestBashEmptyOutputSaysSo(t *testing.T) {
	// An empty result reads as a failed call rather than as mkdir succeeding.
	requireBash(t)
	s := newSession(t, nil)

	text, _, err := run(t, bashTool(), s, map[string]any{"command": "mkdir newdir"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("a silent successful command rendered as an empty string")
	}
	if !strings.Contains(text, "no output") {
		t.Errorf("result = %q", text)
	}
}

func TestBashRespectsTimeoutCeiling(t *testing.T) {
	requireBash(t)
	s := newSession(t, nil)

	tool := bashTool(func(c *config.Shell) {
		c.DefaultTimeout = config.Duration(200 * time.Millisecond)
		c.MaxTimeout = config.Duration(400 * time.Millisecond)
	})

	start := time.Now()
	_, out, err := run(t, tool, s, map[string]any{
		"command": "sleep 30", "timeout_seconds": 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.(*BashResult).TimedOut {
		t.Fatal("the command was not stopped")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a request for 3600s was honoured over the ceiling: took %s", elapsed)
	}
}

func TestBashCanBeDisabled(t *testing.T) {
	s := newSession(t, nil)
	tool := NewBash(workspace.NewLocalShell(), config.Shell{Enabled: false})

	_, err := tool.Execute(context.Background(), s, json.RawMessage(`{"command":"echo hi"}`))
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeDisabled {
		t.Fatalf("err = %v, want TOOL_DISABLED", err)
	}
}

func TestBashRequiresACommand(t *testing.T) {
	s := newSession(t, nil)

	_, err := bashTool().Execute(context.Background(), s, json.RawMessage(`{"command":"   "}`))
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
}

func TestBashIsNotConcurrencySafe(t *testing.T) {
	// A command can do anything, so it is a barrier however harmless it looks.
	if bashTool().ConcurrencySafe([]byte(`{"command":"echo hi"}`)) {
		t.Error("bash must never opt into concurrent execution")
	}
}

func TestBashIsNotJailed(t *testing.T) {
	// Asserting the documented reality rather than a wish. The path jail covers
	// the file tools; a command runs as the server's user with no container.
	// If this ever starts failing, the security posture has changed and
	// docs/security.md and the README are wrong.
	requireBash(t)
	s := newSession(t, nil)

	text, out, err := run(t, bashTool(), s, map[string]any{"command": "cat /etc/hosts | head -1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.(*BashResult).ExitCode != 0 {
		t.Skip("no /etc/hosts on this machine")
	}
	if strings.TrimSpace(text) == "" {
		t.Error("expected to read outside the workspace; if this now fails, the " +
			"documented threat model is out of date")
	}
}

func TestBashChangesAreCaughtByTheLedger(t *testing.T) {
	// A command that rewrites a file the model has read must invalidate that
	// observation — otherwise a later edit applies against remembered contents
	// and silently discards the command's work.
	requireBash(t)
	s := newSession(t, map[string]string{"a.txt": "original\n"})
	observe(t, s, "a.txt")

	if _, _, err := run(t, bashTool(), s, map[string]any{
		"command": "echo rewritten > a.txt",
	}); err != nil {
		t.Fatal(err)
	}

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.txt", "old_string": "original", "new_string": "changed",
	})
	var oe *workspace.ObservationError
	if !errorAs(err, &oe) || oe.Code != workspace.CodeStale {
		t.Fatalf("err = %v, want FS_STALE_VERSION after a shell rewrite", err)
	}
}

func TestBashUntouchedFileStaysEditable(t *testing.T) {
	// The other half: the ledger must not blanket-invalidate after any command,
	// or every `ls` would force a re-read.
	requireBash(t)
	s := newSession(t, map[string]string{"a.txt": "original\n"})
	observe(t, s, "a.txt")

	if _, _, err := run(t, bashTool(), s, map[string]any{"command": "ls"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.txt", "old_string": "original", "new_string": "changed",
	}); err != nil {
		t.Fatalf("an unrelated command invalidated an untouched observation: %v", err)
	}
}
