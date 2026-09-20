#!/usr/bin/env bash
set -uo pipefail
. "$HARNESS_TASK_DIR/../_lib.sh"
require_cmd go

unchanged parse_test.go
go test ./... || fail "the test still fails"

# Total must still skip rather than swallow: a fix that returns 0 for everything
# would pass a test asserting only the total.
cat > ./chain_probe_test.go <<'PROBE'
package pipeline

import "testing"

func TestProbeParseRowReportsRatherThanPanics(t *testing.T) {
	if _, _, err := ParseRow("no-colon-here"); err == nil {
		t.Fatal("ParseRow accepted a row with no colon")
	}
	name, qty, err := ParseRow("bolt: 7")
	if err != nil || name != "bolt" || qty != 7 {
		t.Fatalf("ParseRow(\"bolt: 7\") = %q, %d, %v", name, qty, err)
	}
}
PROBE
go test -run TestProbe ./... || { rm -f ./chain_probe_test.go; fail "ParseRow no longer parses valid rows correctly"; }
rm -f ./chain_probe_test.go
