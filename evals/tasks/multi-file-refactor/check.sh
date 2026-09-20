#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"
require_cmd go

stale="$(grep -rl --exclude-dir=.harness --exclude-dir=.git 'FetchRecord' . || true)"
[ -n "$stale" ] && fail "FetchRecord still appears in: $(echo $stale)"
grep -q 'func LoadRecord' store.go || fail "LoadRecord is not defined in store.go"
grep -q '// LoadRecord' store.go || fail "the doc comment was not renamed with the function"

# Every call site: handler.go has two, main.go has one, the test has two.
for f in handler.go main.go store_test.go; do
  grep -q 'LoadRecord' "$f" || fail "$f does not call LoadRecord"
done

go build ./... || fail "the module does not build"
go test ./... || fail "the tests do not pass"
