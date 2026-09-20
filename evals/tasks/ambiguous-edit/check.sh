#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"
require_cmd python3

grep -q 'matched = 0' stats.py || fail "the local was not renamed to matched"
grep -q 'return matched' stats.py || fail "the return still names the old variable"

# The collateral damage this task exists to catch.
grep -q 'self.count = 0' stats.py || fail "Window.count was renamed too"
grep -q '"count": window.count' stats.py || fail "summarise() was broken by an over-broad rename"

python3 - <<'PY' || fail "the module no longer behaves correctly"
import importlib.util
spec = importlib.util.spec_from_file_location("stats", "stats.py")
m = importlib.util.module_from_spec(spec); spec.loader.exec_module(m)
w = m.Window(3)
for v in (1, 2, 3, 4): w.add(v)
assert w.count == 4, w.count
assert m.summarise(w) == {"count": 4, "mean": 3.0}, m.summarise(w)
assert m.tally([1, None, 2, None]) == 2
assert m.describe([1, None]) == "1 of 2 are set"
PY
