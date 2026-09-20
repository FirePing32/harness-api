package tools

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FirePing32/harness-api/internal/audit"
	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// The audit log is only worth anything if it is actually reached from the path
// commands take. These run the real tool against the real shell.

func TestBashAuditsEveryCommandItRuns(t *testing.T) {
	requireBash(t)
	s := newSession(t, nil)

	var buf bytes.Buffer
	tool := bashTool().WithAudit(audit.NewWriter(&buf))

	if _, _, err := run(t, tool, s, map[string]any{"command": "echo one"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, tool, s, map[string]any{"command": "exit 3"}); err != nil {
		t.Fatal(err)
	}

	records := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(records) != 2 {
		t.Fatalf("got %d records for 2 commands:\n%s", len(records), buf.String())
	}

	var ev audit.Event
	if err := json.Unmarshal([]byte(records[1]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Command != "exit 3" {
		t.Errorf("Command = %q", ev.Command)
	}
	if ev.ExitCode == nil || *ev.ExitCode != 3 {
		t.Errorf("ExitCode = %v, want 3 — a failed command is the one most worth recording", ev.ExitCode)
	}
	if ev.SessionID != s.ID() {
		t.Errorf("SessionID = %q, want %q", ev.SessionID, s.ID())
	}
	if ev.Limits == "" {
		t.Error("the resource ceilings in force were not recorded")
	}
}

func TestBashWithoutAnAuditLogStillWorks(t *testing.T) {
	// The default path. NewBash leaves the logger nil.
	requireBash(t)
	s := newSession(t, nil)

	text, _, err := run(t, bashTool(), s, map[string]any{"command": "echo fine"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "fine") {
		t.Errorf("output = %q", text)
	}
}

func TestWithAuditDoesNotMutateTheOriginal(t *testing.T) {
	// It returns a copy, so a registry built once and audited later cannot
	// retroactively change what an already-registered tool does.
	base := bashTool()
	var buf bytes.Buffer
	audited := base.WithAudit(audit.NewWriter(&buf))

	if base.audit != nil {
		t.Error("WithAudit mutated the receiver")
	}
	if audited.audit == nil {
		t.Error("the copy has no audit logger")
	}
}

func TestBashAppliesResourceLimitsFromConfig(t *testing.T) {
	// Proves the config reaches the kernel, not merely the struct.
	requireBash(t)
	s := newSession(t, nil)

	tool := bashTool(func(c *config.Shell) {
		c.MaxFileSizeKB = 64
		c.CPUFactor = 2 // with a 10s default timeout, a 20s ceiling
	})

	text, out, err := run(t, tool, s, map[string]any{"command": "ulimit -f; ulimit -t"})
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*BashResult); r.ExitCode != 0 {
		t.Fatalf("exit %d: %s", r.ExitCode, text)
	}

	fields := strings.Fields(text)
	if len(fields) < 2 {
		t.Fatalf("output = %q", text)
	}
	if fields[0] != "64" {
		t.Errorf("file size ceiling = %q, want 64", fields[0])
	}
	if fields[1] != "20" {
		t.Errorf("cpu ceiling = %q, want 20 (10s timeout x factor 2)", fields[1])
	}
}

func TestBashLeavesLimitsOffWhenConfigured(t *testing.T) {
	requireBash(t)
	s := newSession(t, nil)

	tool := bashTool(func(c *config.Shell) {
		c.MaxFileSizeKB = 0
		c.MaxProcesses = 0
		c.CPUFactor = 0
	})

	text, _, err := run(t, tool, s, map[string]any{"command": "ulimit -t"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(text); got != "unlimited" {
		t.Errorf("ulimit -t = %q with every ceiling disabled, want unlimited", got)
	}
}

func TestBashExplainsAResourceKillRatherThanShowingABareCode(t *testing.T) {
	// Exit 153 and a truncated file have nothing in them pointing at a size
	// ceiling. A model that cannot reach the cause retries the same command.
	requireBash(t)
	s := newSession(t, nil)

	tool := bashTool(func(c *config.Shell) { c.MaxFileSizeKB = 32 })

	text, out, err := run(t, tool, s,
		map[string]any{"command": "dd if=/dev/zero of=big.bin bs=1024 count=4096 2>/dev/null"})
	if err != nil {
		t.Fatal(err)
	}

	r := out.(*BashResult)
	if r.ExitCode != workspace.ExitFileSizeLimit {
		t.Fatalf("ExitCode = %d, want %d (SIGXFSZ)", r.ExitCode, workspace.ExitFileSizeLimit)
	}
	if !strings.Contains(text, "32 KiB") {
		t.Errorf("the rendered result does not name the ceiling: %q", text)
	}
	if !strings.Contains(text, "truncated") {
		t.Errorf("the rendered result does not warn the partial file is unusable: %q", text)
	}
}
