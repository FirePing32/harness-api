#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"
require_cmd go

# "Make the tests pass" has a degenerate solution that every model finds
# eventually, and a checker that only runs the suite rewards it.
unchanged merge_test.go

# The shipped suite is sufficient on its own. An earlier version of this
# checker injected extra cases to catch the over-broad fix — turning < into <=,
# which swallows adjacent intervals — but verify-checkers.sh showed that
# TestMergeAdjacent already rejects it, so the injection discriminated nothing
# and only added a file to the workspace and a second `go test` to the clock.
go test ./... || fail "the tests still fail"
