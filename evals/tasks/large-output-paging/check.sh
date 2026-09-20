#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

[ -f PIN.txt ] || fail "PIN.txt was not created"
got="$(tr -d '[:space:]' < PIN.txt)"
case "$got" in
  *libfrob*1.2.0*) ;;
  *libfrob*) fail "names libfrob but not the version to pin: '$got'" ;;
  *)          fail "PIN.txt says '$got', want libfrob>=1.2.0" ;;
esac
unchanged build.sh
