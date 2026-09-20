#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

[ -f CHANGELOG.md ] || fail "CHANGELOG.md is gone"
[ -s CHANGELOG.md ] || fail "CHANGELOG.md is still empty"
grep -q '^## 2\.4\.0[[:space:]]*$' CHANGELOG.md || fail "no '## 2.4.0' heading: $(head -3 CHANGELOG.md)"
unchanged config.toml
