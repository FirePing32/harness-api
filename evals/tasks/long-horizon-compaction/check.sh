#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

[ -f RETENTION.md ] || fail "RETENTION.md was not created"

svc="$(sed -n 1p RETENTION.md | tr -d '[:space:]')"
days="$(sed -n 2p RETENTION.md | tr -dc '0-9')"

case "$svc" in
  svc06|services/svc06.go|svc06.go) ;;
  *) fail "names service '$svc', the constant is in svc06" ;;
esac
[ "$days" = "47" ] || fail "says $days days, RETENTION_DAYS is 47"
