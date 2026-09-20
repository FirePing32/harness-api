package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

func runShell(t *testing.T, req ShellRequest) *ShellResult {
	t.Helper()
	if req.Timeout == 0 {
		req.Timeout = 10 * time.Second
	}
	if req.TailBytes == 0 {
		req.TailBytes = 32 << 10
	}
	res, err := NewLocalShell().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("shell run failed: %v", err)
	}
	return res
}

func TestShellCapturesOutputAndExitCode(t *testing.T) {
	requireBash(t)

	res := runShell(t, ShellRequest{Command: "echo out; echo err >&2; exit 7"})

	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.Output, "out") || !strings.Contains(res.Output, "err") {
		t.Errorf("a stream was dropped: %q", res.Output)
	}
}

func TestShellRunsInTheRequestedDirectory(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()

	res := runShell(t, ShellRequest{Command: "pwd", Dir: dir})

	resolved, _ := filepath.EvalSymlinks(dir)
	if got := strings.TrimSpace(res.Output); got != dir && got != resolved {
		t.Errorf("pwd = %q, want %q", got, dir)
	}
}

func TestShellTimeoutKillsBackgroundChildrenThatIgnoreSIGINT(t *testing.T) {
	// The finding this test exists to pin down.
	//
	// POSIX requires a non-interactive shell without job control to set SIGINT
	// to *ignored* in any command it starts in the background. So `sleep 30 &`
	// inside `bash -c` cannot be interrupted by SIGINT at all: measured
	// directly, bash itself dies and the background child carries on running,
	// holding whatever it holds, long after the request returned.
	//
	// If the graceful signal is ever changed back to SIGINT, this fails.
	requireBash(t)

	marker := filepath.Join(t.TempDir(), "survivor")
	// Long enough that only an actual kill prevents the write.
	cmd := "bash -c 'sleep 6; echo alive > " + marker + "' & sleep 30"

	start := time.Now()
	res := runShell(t, ShellRequest{Command: cmd, Timeout: 300 * time.Millisecond})

	if !res.TimedOut {
		t.Error("TimedOut is false after the command was stopped")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("stopping the command took %s", elapsed)
	}

	// Well past when the survivor would have written.
	time.Sleep(7 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a background grandchild outlived the timeout; the process group " +
			"was not terminated")
	}
}

func TestShellTimeoutReportsTimedOutNotJustAnExitCode(t *testing.T) {
	requireBash(t)

	res := runShell(t, ShellRequest{Command: "sleep 30", Timeout: 200 * time.Millisecond})

	if !res.TimedOut {
		t.Fatal("TimedOut is false")
	}
	if res.ExitCode == 0 {
		t.Error("a killed command reported success")
	}
}

func TestShellCancellationIsNotATimeout(t *testing.T) {
	// A client hanging up and a command overrunning its budget need different
	// messages, so they must be distinguishable here.
	requireBash(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	res, err := NewLocalShell().Run(ctx, ShellRequest{
		Command: "sleep 30", Timeout: time.Minute, TailBytes: 1 << 10,
	})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.TimedOut {
		t.Error("a cancelled command was reported as having timed out")
	}
}

func TestShellKeepsTheTailOfLargeOutput(t *testing.T) {
	requireBash(t)

	res := runShell(t, ShellRequest{
		Command:   "for i in $(seq 1 20000); do echo \"line $i\"; done",
		TailBytes: 2 << 10,
	})

	if !res.Truncated {
		t.Fatal("large output was not marked truncated")
	}
	if !strings.Contains(res.Output, "line 20000") {
		t.Error("the end of the output was lost; truncation must drop the head")
	}
	if strings.Contains(res.Output, "line 1\n") {
		t.Error("the beginning survived")
	}
	if len(res.Output) > 4<<10 {
		t.Errorf("kept %d bytes, over the cap", len(res.Output))
	}
	if res.TotalBytes <= len(res.Output) {
		t.Error("TotalBytes should reflect everything the command printed")
	}
}

func TestShellSpillReceivesEverything(t *testing.T) {
	requireBash(t)

	var spill strings.Builder
	res := runShell(t, ShellRequest{
		Command:   "for i in $(seq 1 5000); do echo \"line $i\"; done",
		TailBytes: 1 << 10,
		Spill:     &spill,
	})

	if !strings.Contains(spill.String(), "line 1\n") {
		t.Error("the spill is missing the beginning")
	}
	if !strings.Contains(spill.String(), "line 5000") {
		t.Error("the spill is missing the end")
	}
	if spill.Len() != res.TotalBytes {
		t.Errorf("spill has %d bytes, command printed %d", spill.Len(), res.TotalBytes)
	}
}

func TestShellEnvironmentDoesNotLeakBetweenRuns(t *testing.T) {
	// Each call is a fresh shell. Anything that looks like persistence is a bug.
	requireBash(t)

	runShell(t, ShellRequest{Command: "export LEAKED=yes"})
	res := runShell(t, ShellRequest{Command: `echo "[$LEAKED]"`})

	if !strings.Contains(res.Output, "[]") {
		t.Errorf("an exported variable survived into the next call: %q", res.Output)
	}
}

func TestShellRejectsEmptyCommand(t *testing.T) {
	if _, err := NewLocalShell().Run(context.Background(), ShellRequest{}); err == nil {
		t.Error("an empty command was accepted")
	}
}

func TestTailWriterKeepsTheEnd(t *testing.T) {
	w := newTailWriter(10)
	w.Write([]byte("0123456789abcdef"))

	if got := w.String(); got != "6789abcdef" {
		t.Errorf("String = %q, want the last 10 bytes", got)
	}
	if w.total != 16 {
		t.Errorf("total = %d, want 16", w.total)
	}
	if !w.truncated() {
		t.Error("truncated() is false")
	}
}

func TestTailWriterAcrossManySmallWrites(t *testing.T) {
	w := newTailWriter(8)
	for _, s := range []string{"aaa", "bbb", "ccc", "ddd"} {
		w.Write([]byte(s))
	}

	if got := w.String(); len(got) > 8 {
		t.Errorf("String = %q, longer than the ring", got)
	}
	if !strings.HasSuffix(w.String(), "ddd") {
		t.Errorf("String = %q, want it to end with the last write", w.String())
	}
	if w.total != 12 {
		t.Errorf("total = %d, want 12", w.total)
	}
}

func TestTailWriterUnderCapacityIsExact(t *testing.T) {
	w := newTailWriter(100)
	w.Write([]byte("short output\n"))

	if got := w.String(); got != "short output\n" {
		t.Errorf("String = %q", got)
	}
	if w.truncated() {
		t.Error("truncated() is true for output that fits")
	}
}
