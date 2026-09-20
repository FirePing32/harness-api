#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"
require_cmd go

go build -o /tmp/eval_report_bin . || fail "it still does not compile"

out="$(/tmp/eval_report_bin)" || fail "the program exits non-zero"
rm -f /tmp/eval_report_bin

echo "$out" | grep -q 'alpha' || fail "the report no longer lists its rows: $out"
echo "$out" | grep -q 'total: 10' || fail "the total is wrong or missing: $out"
echo "$out" | grep -q '2 rows' || fail "the row count line is missing: $out"
