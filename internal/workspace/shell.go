package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

// Running commands.
//
// Each call gets a fresh `bash -c`. There is no persistent shell, and the tool
// description tells the model to pass a working directory rather than to `cd`.
// That is the DeepSeek Harness design, and adopting it deletes the hardest code
// in this package: no sentinel framing to find command boundaries in a shared
// stream, no detecting and respawning a shell that died, no pipe bookkeeping,
// and no ambiguity about which command produced which bytes. What a persistent
// shell would preserve — environment variables and the current directory —
// is either passed explicitly or lives in the filesystem, which persists anyway.
//
// Shell is an interface for one reason: the production implementation runs
// commands directly on the host, with no container boundary. If the deployment
// posture ever changes, a sandboxed implementation drops in here without
// touching the tool, the loop, or anything above them. Keeping the seam costs
// nothing today.

// Shell runs a command and returns what it printed.
type Shell interface {
	Run(ctx context.Context, req ShellRequest) (*ShellResult, error)
}

// ShellRequest is one command to run.
type ShellRequest struct {
	// Command is passed to `bash -c` verbatim.
	Command string

	// Dir is the absolute working directory. It is resolved by the caller,
	// because the caller is the only layer that knows about the jail.
	Dir string

	// Timeout bounds the run. Zero means no timeout, which no caller should use.
	Timeout time.Duration

	// TailBytes caps the returned output, keeping the end.
	TailBytes int

	// Spill, if non-nil, receives the complete output regardless of TailBytes.
	Spill io.Writer

	// Env replaces the environment when non-nil.
	Env []string
}

// ShellResult is what a command produced.
type ShellResult struct {
	// Output is the tail of the combined stream, at most TailBytes.
	Output string

	// ExitCode is the process's exit status. A non-zero value is a normal
	// outcome, not an error: a failing test suite is information the model
	// needs, and turning it into a tool failure would hide the output that
	// explains it.
	ExitCode int

	// TotalBytes is how much the command printed in total.
	TotalBytes int

	// Truncated reports that Output is only the tail.
	Truncated bool

	// TimedOut reports that the command was killed rather than finishing.
	TimedOut bool

	Duration time.Duration
}

// ErrShellUnavailable is returned when commands cannot be run at all.
var ErrShellUnavailable = errors.New("no shell is available on this platform")

// tailWriter keeps the last n bytes of a stream and counts the whole thing.
//
// The tail is what matters: the error at the end of a failed build is the one
// thing the model needs, and it sits under however many kilobytes of
// compilation progress. Keeping a ring rather than the whole stream means a
// command that prints a gigabyte costs a fixed amount of memory.
type tailWriter struct {
	buf   []byte
	n     int
	total int
}

func newTailWriter(n int) *tailWriter {
	if n <= 0 {
		n = 32 << 10
	}
	return &tailWriter{n: n}
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.total += len(p)

	if len(p) >= w.n {
		// This write alone overflows the ring; keep only its tail.
		w.buf = append(w.buf[:0], p[len(p)-w.n:]...)
		return len(p), nil
	}

	if len(w.buf)+len(p) > w.n {
		drop := len(w.buf) + len(p) - w.n
		w.buf = append(w.buf[:0], w.buf[drop:]...)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

// String returns the retained tail, starting at a line boundary where one is
// close by, so the first line shown is whole rather than a fragment.
func (w *tailWriter) String() string {
	if w.total <= w.n {
		return string(w.buf)
	}
	if i := bytes.IndexByte(w.buf, '\n'); i >= 0 && i < w.n/2 {
		return string(w.buf[i+1:])
	}
	return string(w.buf)
}

func (w *tailWriter) truncated() bool { return w.total > len(w.buf) }

// LocalShell runs commands directly on the host.
//
// It is not a sandbox and does not pretend to be one. The path jail applies to
// the file tools; a command run here can read and write anything the server's
// user can. The timeout and the output cap exist to stop accidents, not to
// contain an adversary, and docs/security.md says so in as many words.
type LocalShell struct{}

// NewLocalShell builds the production shell.
func NewLocalShell() *LocalShell { return &LocalShell{} }

// Run executes one command.
func (s *LocalShell) Run(ctx context.Context, req ShellRequest) (*ShellResult, error) {
	if req.Command == "" {
		return nil, errors.New("command is empty")
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	tail := newTailWriter(req.TailBytes)
	var out io.Writer = tail
	if req.Spill != nil {
		out = io.MultiWriter(tail, req.Spill)
	}

	cmd := exec.Command("bash", "-c", req.Command)
	cmd.Dir = req.Dir
	if req.Env != nil {
		cmd.Env = req.Env
	}

	// One writer for both streams. Interleaving matters: a build that prints
	// progress to stdout and its error to stderr is only readable if the two
	// arrive in the order they were written.
	cmd.Stdout = out
	cmd.Stderr = out

	setProcessGroup(cmd)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	result := &ShellResult{}
	waitErr := waitOrKill(runCtx, cmd, result)
	result.Duration = time.Since(start)

	result.Output = tail.String()
	result.TotalBytes = tail.total
	result.Truncated = tail.truncated()

	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		result.ExitCode = 0
	case errors.As(waitErr, &exitErr):
		result.ExitCode = exitErr.ExitCode()
		if result.ExitCode < 0 {
			// Killed by a signal. Shells report this as 128 + signal number;
			// matching that keeps the number meaningful to the model.
			result.ExitCode = 137
		}
	default:
		return nil, waitErr
	}

	if runCtx.Err() != nil && ctx.Err() == nil {
		result.TimedOut = true
	}
	return result, nil
}

// gracePeriod is how long a command gets to exit after SIGINT before it is
// killed outright. Long enough for a test runner to print a summary and remove
// its temporary files, short enough not to stall the request.
const gracePeriod = 2 * time.Second

// waitOrKill waits for the command, terminating the whole process group if the
// context ends first.
//
// The group is what matters. `npm test` is a shell that spawns node, which
// spawns workers; signalling only the process bash forked leaves every one of
// them running, holding ports and burning CPU long after the request that
// started them returned.
//
// The shell exiting is not the same as the group being clear, which is the
// subtlety that makes the final sweep necessary rather than defensive. A
// background child can outlive the shell that started it, so SIGKILL goes to
// the group on every timeout path — including the one where bash has already
// gone quietly.
//
// Sending to a group whose members have all exited is harmless: kill returns
// ESRCH and nothing happens. It cannot hit an unrelated group either, because
// a process-group id stays reserved for as long as the group has any member,
// so an id that is still in use is still ours.
func waitOrKill(ctx context.Context, cmd *exec.Cmd, result *ShellResult) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}

	// Ask first, so a test runner can print its summary and clean up.
	signalGroup(cmd, terminateSignal)

	select {
	case err := <-done:
		signalGroup(cmd, killSignal) // sweep anything that outlived the shell
		return err
	case <-time.After(gracePeriod):
	}

	signalGroup(cmd, killSignal)
	return <-done
}
