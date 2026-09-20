#!/usr/bin/env bash
# Simulates a noisy build. The interesting line is a long way from the end.
set -euo pipefail
for i in $(seq 1 6000); do
  echo "[$i/6000] compiling module_$i.o ... ok"
  if [ "$i" -eq 2500 ]; then
    echo "WARNING: module_2500 links against libfrob 0.9.3, which is deprecated; pin libfrob>=1.2.0"
  fi
done
echo "build finished in 41.2s"
