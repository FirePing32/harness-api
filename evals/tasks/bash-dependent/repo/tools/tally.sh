#!/usr/bin/env bash
# Sums the second column of every readings file and prints the total.
set -euo pipefail
cd "$(dirname "$0")/.."
awk -F, '{ sum += $2 } END { printf "%d\n", sum }' data/*.csv
