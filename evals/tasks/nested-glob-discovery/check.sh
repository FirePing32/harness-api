#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

[ -f COUNT.txt ] || fail "COUNT.txt was not created"
got="$(tr -dc '0-9' < COUNT.txt)"
case "$got" in
  3) ;;
  5) fail "counted 5: vendor/ and node_modules/ are ignored by .gitignore" ;;
  *) fail "COUNT.txt says '$got', want 3" ;;
esac
