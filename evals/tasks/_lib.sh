# Shared helpers for task checkers. Source it:
#
#   . "$HARNESS_TASK_DIR/../_lib.sh"
#
# Checkers run with the agent's workspace as their working directory. The
# environment carries everything else:
#
#   HARNESS_WORKSPACE   the workspace (also the cwd)
#   HARNESS_TASK_DIR    the task's own directory, including the pristine repo/
#   HARNESS_HIDDEN_DIR  files deliberately withheld from the agent
#   HARNESS_ANSWER      the agent's final message, as a file
#   HARNESS_EVENTS      one JSON event per line: every tool call and its outcome
#
# Exit 0 to pass, 1 to fail, and 99 to say the check itself could not run. That
# third code matters: a missing `go` on the box is not the agent failing a task,
# and a suite that cannot tell those apart will report a broken toolchain as a
# capability regression.

BROKEN=99

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

broken() {
	echo "CHECKER BROKEN: $*" >&2
	exit $BROKEN
}

require_cmd() {
	command -v "$1" >/dev/null 2>&1 || broken "$1 is not installed"
}

# pristine <relpath> — the file as the agent first saw it.
pristine() {
	echo "$HARNESS_TASK_DIR/repo/$1"
}

# unchanged <relpath> — fail if the agent touched this file.
#
# Used on test files. "Make the tests pass" has a degenerate solution that
# every model finds eventually, and a checker that only runs the suite rewards
# it.
unchanged() {
	local rel="$1"
	local original
	original="$(pristine "$rel")"
	[ -f "$original" ] || broken "no pristine copy of $rel"
	[ -f "$rel" ] || fail "$rel was deleted"
	cmp -s "$rel" "$original" || fail "$rel was modified; it was not the agent's to change"
}

# answer_matches <extended-regex> — grep the agent's final message.
#
# Used sparingly. Prose is not a reliable signal and matching on it drifts
# towards grading style. Where a fact can be checked in the filesystem instead,
# it is checked there.
answer_matches() {
	[ -f "$HARNESS_ANSWER" ] || fail "no answer was recorded"
	grep -Eqi -- "$1" "$HARNESS_ANSWER" || fail "the answer does not match /$1/"
}

# tool_was_used <name> — did the agent call this tool at all?
tool_was_used() {
	[ -f "$HARNESS_EVENTS" ] || broken "no event log"
	grep -q "\"tool\":\"$1\"" "$HARNESS_EVENTS"
}
