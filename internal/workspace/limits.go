package workspace

import (
	"fmt"
	"strings"
)

// Resource ceilings for a command.
//
// The plan for this said "setrlimit on CPU/FSIZE/NPROC/AS via SysProcAttr".
// Go's SysProcAttr carries no rlimit fields on any unix, and there is no
// pre-exec hook to call setrlimit in the child, so that route does not exist.
// What does exist is the shell's own ulimit, applied by a wrapper that execs
// the real shell:
//
//	sh -c 'ulimit -t N; ulimit -f K; exec "$0" "$@"' bash -c <command>
//
// The model's command stays a separate argv element throughout, so it is never
// re-parsed, quoting survives intact, and a syntax error still reports the
// line number the model would expect. Prefixing the limits onto the command
// text would shift every line number by the size of the prelude.
//
// Limits set this way cannot be lifted. Bash's bare `ulimit -t N` sets the soft
// and hard limits together, and lowering a hard limit is irreversible for a
// non-root process — verified, not assumed: a child that tries gets "Operation
// not permitted". (Setting the hard limit alone fails with EINVAL, because it
// would leave soft above hard.)

// Limits are the resource ceilings applied to one command.
type Limits struct {
	// CPUSeconds bounds consumed processor time. Zero means no limit.
	//
	// This is not a second wall clock. It exists for the one case the timeout
	// cannot cover: a process that leaves its process group by double-forking
	// or calling setsid survives the group kill, and nothing else in this
	// server will ever stop it. An rlimit is inherited across fork and exec, so
	// it follows the escapee.
	CPUSeconds int

	// FileSizeKB bounds any single file the command writes. Zero means no
	// limit.
	//
	// The output cap protects what is read back into the conversation; it does
	// nothing about `yes > junk`, which fills the disk and takes the rest of
	// the machine down with it.
	FileSizeKB int

	// Processes bounds concurrent processes. Zero means no limit, and zero is
	// the default deliberately — see docs/security.md. RLIMIT_NPROC counts
	// every process belonging to the real user id, not the ones this command
	// started, so a value chosen for a dedicated service account will refuse
	// the very first fork on a shared login that already has hundreds.
	Processes int
}

// Empty reports whether no limit is set.
func (l Limits) Empty() bool {
	return l.CPUSeconds <= 0 && l.FileSizeKB <= 0 && l.Processes <= 0
}

// prelude renders the ulimit commands. Each is guarded with `|| true` so that
// a platform refusing one limit does not abort the command: a kernel that will
// not apply a ceiling is a reason to run with less protection, not a reason to
// fail work the user asked for.
func (l Limits) prelude() string {
	var parts []string
	if l.CPUSeconds > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -t %d 2>/dev/null || true", l.CPUSeconds))
	}
	if l.FileSizeKB > 0 {
		// Bash's -f unit is 1024-byte blocks.
		parts = append(parts, fmt.Sprintf("ulimit -f %d 2>/dev/null || true", l.FileSizeKB))
	}
	if l.Processes > 0 {
		parts = append(parts, fmt.Sprintf("ulimit -u %d 2>/dev/null || true", l.Processes))
	}
	return strings.Join(parts, "; ")
}

// Argv builds the command line for a shell invocation under these limits.
//
// With no limits set it is the plain `bash -c <command>` this server has always
// run, so the wrapper is not in the path at all when it would do nothing.
func (l Limits) Argv(command string) []string {
	if l.Empty() {
		return []string{"bash", "-c", command}
	}
	return []string{"sh", "-c", l.prelude() + `; exec "$0" "$@"`, "bash", "-c", command}
}

// Describe renders the limits for a log line or an error message.
func (l Limits) Describe() string {
	if l.Empty() {
		return "none"
	}
	var parts []string
	if l.CPUSeconds > 0 {
		parts = append(parts, fmt.Sprintf("cpu=%ds", l.CPUSeconds))
	}
	if l.FileSizeKB > 0 {
		parts = append(parts, fmt.Sprintf("fsize=%dKiB", l.FileSizeKB))
	}
	if l.Processes > 0 {
		parts = append(parts, fmt.Sprintf("nproc=%d", l.Processes))
	}
	return strings.Join(parts, " ")
}

// Exit codes a shell reports when the kernel enforces one of these limits.
// Both are 128 plus the signal number, which is how a shell reports any
// signal death.
const (
	// ExitCPULimit is SIGXCPU (24).
	ExitCPULimit = 152
	// ExitFileSizeLimit is SIGXFSZ (25).
	ExitFileSizeLimit = 153
)

// ExplainExit returns a note for an exit code produced by a resource limit, or
// "" for anything else.
//
// Without this the model sees exit 153 and a truncated file, with nothing
// connecting the two. It has no way to reach "I hit a file size ceiling" from
// that, so it retries the same command, gets the same result, and burns the
// iteration budget on it.
func (l Limits) ExplainExit(code int) string {
	switch code {
	case ExitCPULimit:
		return fmt.Sprintf("The command was killed for exceeding its CPU limit of %d seconds. "+
			"This usually means a loop that does not terminate. Note that it is processor "+
			"time across all cores, not elapsed time.", l.CPUSeconds)
	case ExitFileSizeLimit:
		return fmt.Sprintf("The command was killed for trying to write a file larger than "+
			"%d KiB. Any file it was writing is truncated at that size and is very likely "+
			"unusable. Write less, or split the output across files.", l.FileSizeKB)
	}
	return ""
}
