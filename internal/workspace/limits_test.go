package workspace

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// These tests run real commands against a real kernel. A test that only
// checked the generated argv would pass just as happily against a limit the
// platform silently ignores, which is the thing actually worth knowing.

func shellTest(t *testing.T) (*LocalShell, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no ulimit")
	}
	return NewLocalShell(), t.TempDir()
}

func TestLimitsArgvKeepsTheCommandUnparsed(t *testing.T) {
	// The command must stay a separate argv element. Prefixing the ulimits onto
	// the command text would shift every line number in a model-written script,
	// so a syntax error would be reported against the wrong line.
	got := Limits{CPUSeconds: 5}.Argv(`echo "a; b | c $X"`)

	if got[len(got)-1] != `echo "a; b | c $X"` {
		t.Errorf("the command was altered: %q", got[len(got)-1])
	}
	if got[len(got)-2] != "-c" || got[len(got)-3] != "bash" {
		t.Errorf("the command is not run by bash -c: %v", got)
	}

	// The wrapper must be bash too. `ulimit -f` counts blocks and the block
	// size is shell-dependent — bash 1024, dash 512 — so a wrapper that is not
	// the same shell as the command enforces a different number than it was
	// given. With /bin/sh on Ubuntu, where that is dash, every file-size
	// ceiling was applied at half its configured value and the behavioural
	// test still passed. Reproduced with dash directly before this was changed.
	if got[0] != "bash" {
		t.Errorf("wrapper is %q, not bash: the shell that sets a block-counted "+
			"limit must be the shell that runs under it", got[0])
	}
}

func TestNoLimitsMeansNoWrapper(t *testing.T) {
	// With nothing to apply, the wrapper should not be in the path at all.
	got := Limits{}.Argv("echo hi")
	if len(got) != 3 || got[0] != "bash" {
		t.Errorf("Argv = %v, want plain bash -c", got)
	}
}

func TestLimitsPreserveQuotingAndLineNumbers(t *testing.T) {
	shell, dir := shellTest(t)
	script := "true\ntrue\nnosuchcommand-xyz"

	run := func(limits Limits) string {
		res, err := shell.Run(context.Background(), ShellRequest{
			Command: script, Dir: dir, Timeout: 20 * time.Second,
			TailBytes: 4096, Limits: limits,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Output
	}

	bare := run(Limits{})
	limited := run(Limits{CPUSeconds: 30, FileSizeKB: 4096})

	// Byte-identical is the property worth asserting. Pinning a specific line
	// number would only test bash's counting convention, which differs between
	// versions and is not what the wrapper could break.
	if !strings.Contains(bare, "line ") {
		t.Fatalf("baseline reports no line number at all: %q", bare)
	}
	if bare != limited {
		t.Errorf("the wrapper changed the diagnostic:\n bare    %q\n limited %q", bare, limited)
	}
}

func TestCPULimitStopsALoopThatEscapedTheTimeout(t *testing.T) {
	// The case the wall clock cannot cover. An rlimit is inherited across fork
	// and exec, so it follows a process that left its group and survived the
	// group kill.
	shell, dir := shellTest(t)

	start := time.Now()
	res, err := shell.Run(context.Background(), ShellRequest{
		Command: "while :; do :; done",
		Dir:     dir,
		// Generous, so that what stops this is demonstrably the CPU ceiling and
		// not the timeout.
		Timeout:   60 * time.Second,
		TailBytes: 4096,
		Limits:    Limits{CPUSeconds: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	if res.TimedOut {
		t.Fatal("the timeout fired; this test proves nothing about the CPU limit")
	}
	if res.ExitCode != ExitCPULimit {
		t.Errorf("ExitCode = %d, want %d (SIGXCPU)", res.ExitCode, ExitCPULimit)
	}
	if elapsed > 30*time.Second {
		t.Errorf("took %s; the limit did not apply", elapsed)
	}
}

func TestFileSizeLimitStopsARunawayWrite(t *testing.T) {
	// The output cap protects the conversation and does nothing about a command
	// filling the disk.
	shell, dir := shellTest(t)

	res, err := shell.Run(context.Background(), ShellRequest{
		Command:   "dd if=/dev/zero of=big.bin bs=1024 count=4096 2>/dev/null",
		Dir:       dir,
		Timeout:   30 * time.Second,
		TailBytes: 4096,
		Limits:    Limits{FileSizeKB: 64},
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.ExitCode == 0 {
		t.Fatal("a write far past the ceiling succeeded")
	}

	info, err := os.Stat(filepath.Join(dir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}

	// Bounded tightly on both sides, because `ulimit -f` counts blocks and the
	// block size differs between shells — bash uses 1024 bytes, dash uses 512.
	// An earlier version of this test allowed twice the ceiling as slack, which
	// is precisely the size of the error it was supposed to catch: with /bin/sh
	// as the wrapper on Ubuntu, every limit was enforced at half its value and
	// this test passed anyway.
	const want = 64 << 10
	if info.Size() > want {
		t.Errorf("wrote %d bytes under a %d-byte ceiling", info.Size(), want)
	}
	if info.Size() < want/2 {
		t.Errorf("wrote only %d bytes under a %d-byte ceiling; the limit is being "+
			"applied in the wrong unit", info.Size(), want)
	}
}

func TestCPULimitLeavesRoomBetweenSoftAndHard(t *testing.T) {
	// The soft limit is the one meant to fire, because SIGXCPU is
	// distinguishable and SIGKILL is not — 137 is also what the timeout sweep
	// produces. With soft equal to hard, Linux reports the SIGKILL and nothing
	// downstream can tell a processor-time kill from a timeout.
	shell, dir := shellTest(t)

	res, err := shell.Run(context.Background(), ShellRequest{
		Command:   "ulimit -S -t; ulimit -H -t",
		Dir:       dir,
		Timeout:   20 * time.Second,
		TailBytes: 4096,
		Limits:    Limits{CPUSeconds: 60},
	})
	if err != nil {
		t.Fatal(err)
	}

	fields := strings.Fields(res.Output)
	if len(fields) < 2 {
		t.Fatalf("output = %q", res.Output)
	}
	if fields[0] != "60" {
		t.Errorf("soft limit = %q, want 60", fields[0])
	}
	if fields[1] == fields[0] {
		t.Errorf("hard limit equals soft (%q); SIGXCPU and SIGKILL will arrive "+
			"together and the kill becomes indistinguishable from a timeout", fields[1])
	}
}

func TestFileSizeCeilingIsSetAndReadInTheSameUnit(t *testing.T) {
	// The wrapper is bash, not sh, so that the shell setting the block-counted
	// limit and the shell running under it agree on what a block is.
	shell, dir := shellTest(t)

	res, err := shell.Run(context.Background(), ShellRequest{
		Command:   "ulimit -f",
		Dir:       dir,
		Timeout:   20 * time.Second,
		TailBytes: 4096,
		Limits:    Limits{FileSizeKB: 4096},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(res.Output); got != "4096" {
		t.Errorf("ulimit -f reports %q for a 4096 KiB ceiling; the wrapper and the "+
			"command disagree about block size", got)
	}
}

func TestALimitedCommandThatBehavesIsUnaffected(t *testing.T) {
	// The important negative. A ceiling that interferes with ordinary work is
	// worse than no ceiling: the model cannot tell a policy refusal from a bug,
	// so it rephrases the same command instead of adapting.
	shell, dir := shellTest(t)

	res, err := shell.Run(context.Background(), ShellRequest{
		Command:   `printf 'hello\n'; mkdir -p a/b && echo nested > a/b/f.txt && cat a/b/f.txt`,
		Dir:       dir,
		Timeout:   30 * time.Second,
		TailBytes: 4096,
		Limits:    Limits{CPUSeconds: 120, FileSizeKB: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d: %s", res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "hello") || !strings.Contains(res.Output, "nested") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestLimitsCannotBeLiftedByTheCommand(t *testing.T) {
	// Bash's bare `ulimit -t N` sets soft and hard together, and lowering a
	// hard limit is irreversible for a non-root process. If that were not so,
	// any command could simply raise the ceiling and the whole mechanism would
	// be decorative.
	shell, dir := shellTest(t)

	res, err := shell.Run(context.Background(), ShellRequest{
		Command:   "ulimit -t 9999 2>/dev/null; ulimit -t",
		Dir:       dir,
		Timeout:   20 * time.Second,
		TailBytes: 4096,
		Limits:    Limits{CPUSeconds: 30},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(res.Output); got != "30" {
		t.Errorf("the command raised its own ceiling: ulimit -t reports %q, want 30", got)
	}
}

func TestExplainExitTurnsASignalIntoSomethingActionable(t *testing.T) {
	// Exit 153 and a truncated file have nothing in them that points at a file
	// size ceiling. A model that cannot reach the cause retries the same
	// command until the iteration budget runs out.
	limits := Limits{CPUSeconds: 60, FileSizeKB: 512}

	cpu := limits.ExplainExit(ExitCPULimit)
	if !strings.Contains(cpu, "60 seconds") {
		t.Errorf("the CPU note does not state the ceiling: %q", cpu)
	}
	if !strings.Contains(cpu, "not elapsed time") {
		t.Errorf("the CPU note does not distinguish processor time from wall clock: %q", cpu)
	}

	fsize := limits.ExplainExit(ExitFileSizeLimit)
	if !strings.Contains(fsize, "512 KiB") {
		t.Errorf("the file size note does not state the ceiling: %q", fsize)
	}
	if !strings.Contains(fsize, "truncated") {
		t.Errorf("the file size note does not warn that the partial file is unusable: %q", fsize)
	}

	if note := limits.ExplainExit(1); note != "" {
		t.Errorf("an ordinary failure was explained as a limit: %q", note)
	}
}

func TestLimitsSurviveAPlatformThatRefusesThem(t *testing.T) {
	// Every ulimit is guarded with `|| true`. A kernel that will not apply a
	// ceiling is a reason to run with less protection, not a reason to fail
	// work the user asked for — and macOS does refuse some of these.
	shell, dir := shellTest(t)

	res, err := shell.Run(context.Background(), ShellRequest{
		Command:   "echo survived",
		Dir:       dir,
		Timeout:   20 * time.Second,
		TailBytes: 4096,
		// Absurd values, the kind a kernel is most likely to reject outright.
		Limits: Limits{CPUSeconds: 1 << 30, FileSizeKB: 1 << 30, Processes: 1 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "survived") {
		t.Errorf("a rejected limit broke the command: exit %d, %q", res.ExitCode, res.Output)
	}
}
