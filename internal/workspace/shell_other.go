//go:build !unix

package workspace

import (
	"os"
	"os/exec"
)

// Windows has no process groups in the POSIX sense and no bash to run. The
// server builds, and the shell tool reports plainly that it is unavailable
// rather than half-working: a timeout that cannot kill the process tree it
// started is worse than no timeout at all.

var (
	terminateSignal = os.Kill
	killSignal      = os.Kill
)

func setProcessGroup(*exec.Cmd) {}

func signalGroup(cmd *exec.Cmd, sig os.Signal) {
	if cmd.Process != nil {
		_ = cmd.Process.Signal(sig)
	}
}

// signalExitCode has no meaning here: Windows has no wait-status signals.
func signalExitCode(*os.ProcessState) int { return 0 }
