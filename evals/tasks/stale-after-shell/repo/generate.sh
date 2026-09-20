#!/usr/bin/env bash
# Regenerates version.txt from config.toml. Run this before editing anything
# that depends on the version.
set -euo pipefail
cd "$(dirname "$0")"
v="$(grep '^version' config.toml | sed 's/.*"\(.*\)".*/\1/')"
printf 'GENERATED\nversion=%s\nbuild=release\n' "$v" > version.txt
