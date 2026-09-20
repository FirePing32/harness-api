#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"
require_cmd go

grep -q 'panic("not implemented")' semver.go && fail "Compare still panics"

# The agent never saw this file.
cp "$HARNESS_HIDDEN_DIR/semver_test.go" ./semver_hidden_test.go || broken "the hidden test is missing"
go test ./... || fail "the hidden test does not pass"
rm -f ./semver_hidden_test.go
