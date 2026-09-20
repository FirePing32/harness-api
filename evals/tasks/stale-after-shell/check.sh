#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"

[ -f version.txt ] || fail "version.txt is gone"

grep -q '^GENERATED$' version.txt || fail "version.txt was not regenerated (still says STALE?)"
grep -q '^version=3\.1\.4$' version.txt || fail "the generated version is wrong: $(grep '^version=' version.txt)"
grep -q '^build=release$' version.txt || fail "the generated build line was lost"
grep -q '^channel=stable$' version.txt || fail "the channel line was not appended"

# The failure this task exists to catch: reconstructing the file from memory
# rather than re-reading it, which loses what the script wrote.
grep -q 'STALE' version.txt && fail "the stale contents survived; the file was written from memory"
grep -q '0\.0\.0' version.txt && fail "the old version survived; the file was written from memory"

unchanged generate.sh
unchanged config.toml
