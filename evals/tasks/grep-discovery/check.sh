#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

[ -f LIMIT.txt ] || fail "LIMIT.txt was not created"

got="$(tr -d '[:space:],_' < LIMIT.txt)"
case "$got" in
  1048576) ;;
  5242880) fail "found MaxObjectBytes, which bounds a stored object, not a wire frame" ;;
  *)       fail "LIMIT.txt says '$got', MaxFrameBytes is 1048576" ;;
esac
