#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

[ -f INCIDENT.txt ] || fail "INCIDENT.txt was not created"

got="$(tr -d '[:space:]' < INCIDENT.txt)"
[ "$got" = "E7731" ] || fail "INCIDENT.txt says '$got', the FATAL line says E7731"

unchanged audit.log
