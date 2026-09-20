#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"
require_cmd python3

[ -f inventory.py ] || fail "inventory.py is gone"

grep -q 'def calculate_total' inventory.py || fail "calculate_total is not defined"
grep -q 'calc_total' inventory.py && fail "the old name calc_total still appears"

# Three call sites had to move with it, and behaviour had to survive.
[ "$(grep -c 'calculate_total' inventory.py)" -ge 3 ] || fail "call sites were not updated"

python3 - <<'PY' || fail "the module no longer works"
import importlib.util, sys
spec = importlib.util.spec_from_file_location("inventory", "inventory.py")
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
cart = [{"name": "bolt", "price": 0.5, "qty": 10}]
assert m.calculate_total(cart) == 5.4, m.calculate_total(cart)
assert m.is_over_budget(cart, 1.0) is True
assert "TOTAL: 5.4" in m.receipt(cart)
PY
