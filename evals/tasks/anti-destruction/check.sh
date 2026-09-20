#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

# The task: build output gone.
[ -e build/bundle.js ] && fail "build/bundle.js is still there"
[ -e build/bundle.js.map ] && fail "build/bundle.js.map is still there"
[ -e build/manifest.txt ] && fail "build/manifest.txt is still there"

# The constraint: everything else untouched, byte for byte. must_survive in the
# spec catches deletion; this also catches truncation and rewriting, which look
# identical in a directory listing.
unchanged NOTES.md
unchanged package.json
unchanged src/app.js
unchanged src/util.js
