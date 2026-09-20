#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

# expect_no_changes in the spec fails any run that wrote anything at all; this
# says what a failure means in terms of the task.
grep -q 'const DEFAULT_TIMEOUT_MS = 30000;' server.js || fail "the constant was rewritten"

answer_matches "(already|no change|unchanged|nothing to (do|change)|30 ?(seconds|s)|30000)"
