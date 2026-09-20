package guard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// RepeatTool denies a call that has already been made, identically, several
// times in the same request.
//
// The loop it catches is specific and common: the model calls a tool, misreads
// or disbelieves the result, and calls it again with exactly the same
// arguments. Nothing errors. Each call looks reasonable on its own. It only
// resolves when the iteration ceiling fires, minutes and many tokens later,
// and the user gets a truncated answer for a task that was never progressing.
//
// Identical arguments are the signal. Reading the same file twice with
// different offsets is normal work; reading it twice with the same offset
// cannot produce anything new.
func RepeatTool(threshold int) Guard {
	if threshold < 2 {
		threshold = 3
	}

	return New("repeat-tool", func(ex Execution) string {
		args := canonicalArgs(ex.Args)

		n := 0
		for _, prior := range ex.Prior {
			if prior.Tool == ex.Tool && canonicalArgs([]byte(prior.Args)) == args {
				n++
			}
		}
		if n < threshold {
			return ""
		}

		return fmt.Sprintf(
			"This is call %d to %s with exactly these arguments in this request, and the "+
				"result will be the same as the previous ones. Repeating it cannot make "+
				"progress.\n"+
				"Something about the current approach is not working. Change the arguments, "+
				"use a different tool, or explain what is blocking you and stop.",
			n+1, ex.Tool)
	})
}

// canonicalArgs normalises an argument string so that two calls differing only
// in key order or whitespace are recognised as the same call. Providers do not
// guarantee stable key ordering between turns, so comparing the raw strings
// would miss most real loops.
func canonicalArgs(raw []byte) string {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return strings.TrimSpace(string(raw))
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(out)
}

// DefaultDeniedCommands are shell commands that are almost never what anyone
// meant.
//
// This is ergonomics, not security, and the distinction is not a hedge. The
// patterns match command text, and command text has unlimited ways to say the
// same thing: r”m -rf /, $(echo rm) -rf /, a variable holding the path, a
// script that does it. Anything trying to get past this gets past it.
//
// What it does catch is the accident — a model that constructs a path badly
// and ends up with `rm -rf /` where it meant `rm -rf /tmp/build/`. That is a
// real failure mode with a catastrophic result and a trivial check, which is
// why it is here despite being bypassable.
// cmdStart matches a position where a command actually begins: the start of
// the text, or just after a separator, optionally through sudo.
//
// Anchoring on it is not a refinement, it is the difference between a usable
// guard and an unusable one. Without it, `echo 'shutdown the server' >> notes.md`
// matches the shutdown rule — the dangerous word is inside a quoted string and
// has nothing to do with what the command does. A guard that blocks ordinary
// work to catch an accident is a worse trade than not having the guard, since
// the model cannot tell a policy refusal from a bug and will keep rephrasing.
const cmdStart = `(?:^|[\n;&|])\s*(?:sudo\s+)?`

// rmRecursive matches rm with a recursive flag, however the flags are spelled.
const rmRecursive = `rm\s+(?:-[a-zA-Z]+\s+)*-[a-zA-Z]*[rR][a-zA-Z]*\s+`

var DefaultDeniedCommands = []string{
	// Recursive deletion of the root, home, or a bare variable that expanded
	// to nothing — the classic `rm -rf "$DIR/"` with DIR unset.
	cmdStart + rmRecursive + `/\s*$`,
	cmdStart + rmRecursive + `/\s`,
	cmdStart + rmRecursive + `~\s*/?\s*$`,
	cmdStart + rmRecursive + `\$\w+\s*/`,

	// Fork bomb. Distinctive enough not to need anchoring.
	`:\s*\(\s*\)\s*\{.*\|.*&.*\}\s*;\s*:`,

	// Piping a download straight into a shell.
	cmdStart + `(?:curl|wget)\b[^|]*\|\s*(?:sudo\s+)?(?:ba|z|k)?sh\b`,

	// Writing over block devices and filesystems.
	cmdStart + `mkfs(?:\.\w+)?\b`,
	cmdStart + `dd\b[^\n]*\bof=/dev/`,
	`>\s*/dev/(?:sd|nvme|disk|hd)`,

	// Taking the machine down.
	cmdStart + `(?:shutdown|reboot|halt|poweroff)\b`,

	// Opening up the whole filesystem.
	cmdStart + `chmod\s+(?:-[a-zA-Z]+\s+)*777\s+/\s*$`,
	cmdStart + `chown\s+-R\s+[^\s]+\s+/\s*$`,
}

// CommandDenylist denies shell commands matching any of the given patterns.
//
// Patterns are RE2. An invalid pattern is skipped rather than fatal: a typo in
// an operator's config should narrow what is blocked, not stop the server from
// starting.
func CommandDenylist(patterns []string) (Guard, []error) {
	var (
		compiled []*regexp.Regexp
		errs     []error
	)
	for _, p := range patterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			errs = append(errs, fmt.Errorf("denied command pattern %q: %w", p, err))
			continue
		}
		compiled = append(compiled, re)
	}

	g := New("command-denylist", func(ex Execution) string {
		if ex.Tool != "bash" {
			return ""
		}
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(ex.Args, &args); err != nil || args.Command == "" {
			return ""
		}

		for _, re := range compiled {
			if !re.MatchString(args.Command) {
				continue
			}
			return "This command is on the server's denied list because it is " +
				"destructive and almost never intended.\n" +
				"If you meant to operate on something inside the workspace, write the " +
				"path out explicitly and scope the command to it. If you believe this " +
				"is a false positive, say what you are trying to achieve instead of " +
				"rephrasing the command."
		}
		return ""
	})
	return g, errs
}

// TimeoutPolicy denies a command that cannot finish inside what is left of the
// request's wall-clock budget.
//
// Starting a five-minute test suite with forty seconds left does not fail
// gracefully: the command is killed part-way, the model sees a timeout, and
// the work is thrown away along with whatever it had already done. Refusing up
// front costs nothing and says something the model can act on.
func TimeoutPolicy() Guard {
	return New("timeout-policy", func(ex Execution) string {
		if ex.Tool != "bash" || ex.Remaining <= 0 {
			return ""
		}

		var args struct {
			TimeoutSeconds int `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(ex.Args, &args); err != nil {
			return ""
		}

		// Only an explicit request is checked. A command relying on the default
		// timeout is left alone: the default is short, and denying it would
		// make the last stretch of every long request unusable.
		if args.TimeoutSeconds <= 0 {
			return ""
		}

		requested := time.Duration(args.TimeoutSeconds) * time.Second
		if requested <= ex.Remaining {
			return ""
		}

		return fmt.Sprintf(
			"This command asks for %s but only %s remain in this request's time budget, "+
				"so it would be killed part-way and its work lost.\n"+
				"Run something smaller that fits — a single test rather than the whole "+
				"suite — or finish up and report what you have.",
			requested.Round(time.Second), ex.Remaining.Round(time.Second))
	})
}
