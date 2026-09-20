//go:build unix

package workspace

import (
	"os"
	"os/exec"
	"syscall"
)

// On unix the child is made a process-group leader, so signals can be
// addressed to the whole tree it goes on to create rather than just to bash.

// The graceful signal is SIGTERM, not SIGINT, and the difference is not
// cosmetic. POSIX requires a non-interactive shell without job control to set
// SIGINT and SIGQUIT to *ignored* in any command it runs in the background, so
// `npm test &` produces a process that cannot be interrupted by SIGINT at all.
// Measured directly: with SIGINT, bash itself dies and its background children
// carry on running; with SIGTERM the whole group goes. SIGTERM is also what
// every test runner and process supervisor already handles as "shut down
// cleanly", which is exactly the behaviour wanted during the grace period.
var (
	terminateSignal = syscall.SIGTERM
	killSignal      = syscall.SIGKILL
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to the command's entire process group.
//
// The negative pid is the whole point: syscall.Kill with a negative value
// addresses the group. Signalling cmd.Process alone would leave every child
// bash spawned still running.
func signalGroup(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process == nil {
		return
	}
	s, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	// Setpgid made the child its own group leader, so the group id is its pid.
	if err := syscall.Kill(-cmd.Process.Pid, s); err != nil {
		// The group may already be gone, which is the common case and not
		// worth reporting. Fall back to the process itself in case the
		// process group was never established.
		_ = cmd.Process.Signal(sig)
	}
}
