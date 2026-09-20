package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func decode(t *testing.T, line string) Event {
	t.Helper()
	var ev Event
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		t.Fatalf("not JSON: %q (%v)", line, err)
	}
	return ev
}

func lines(s string) []string {
	trimmed := strings.TrimRight(s, "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestNilLoggerDiscardsWithoutPanicking(t *testing.T) {
	// The default. Every call site would otherwise need a nil check, and a call
	// site that needs one is a call site that will eventually forget.
	var l *Logger

	l.Command("ws_1", "/tmp/w", "ls", ".", 0, false, time.Second, "none")
	l.Denied("ws_1", "bash", "command-denylist", "no")
	l.Session("ws_1", "/tmp/w", "created")
	l.Write(Event{Kind: KindCommand})

	if l.Enabled() {
		t.Error("a nil Logger reports itself enabled")
	}
	if l.Path() != "" {
		t.Error("a nil Logger has a path")
	}
	if err := l.Close(); err != nil {
		t.Errorf("Close on a nil Logger: %v", err)
	}
}

func TestCommandRecordsWhatWouldBeNeededAfterwards(t *testing.T) {
	var buf bytes.Buffer
	NewWriter(&buf).Command(
		"ws_abc", "/srv/work", "rm -rf build && make", "src",
		2, false, 1500*time.Millisecond, "cpu=480s fsize=1048576KiB")

	ev := decode(t, strings.TrimSpace(buf.String()))
	if ev.Kind != KindCommand {
		t.Errorf("Kind = %q", ev.Kind)
	}
	if ev.Command != "rm -rf build && make" {
		t.Errorf("Command = %q", ev.Command)
	}
	if ev.SessionID != "ws_abc" || ev.Workspace != "/srv/work" || ev.Workdir != "src" {
		t.Errorf("provenance is incomplete: %+v", ev)
	}
	if ev.ExitCode == nil || *ev.ExitCode != 2 {
		t.Errorf("ExitCode = %v", ev.ExitCode)
	}
	if ev.DurationMS != 1500 {
		t.Errorf("DurationMS = %d", ev.DurationMS)
	}
	if ev.Limits == "" {
		t.Error("the limits in force were not recorded")
	}
	if _, err := time.Parse(time.RFC3339Nano, ev.Time); err != nil {
		t.Errorf("Time = %q, not RFC3339: %v", ev.Time, err)
	}
}

func TestExitCodeZeroIsRecordedRatherThanOmitted(t *testing.T) {
	// A pointer, precisely so that "succeeded" and "we did not record the
	// outcome" are different states in the file. With a plain int and
	// omitempty they would be the same JSON.
	var buf bytes.Buffer
	NewWriter(&buf).Command("ws", "/w", "true", ".", 0, false, time.Second, "none")

	if !strings.Contains(buf.String(), `"exit_code":0`) {
		t.Errorf("a successful command has no exit code in the record: %s", buf.String())
	}
}

func TestCommandsAreRecordedVerbatim(t *testing.T) {
	// The deliberate asymmetry with the operational log, which redacts. A
	// record of approximately what ran cannot answer the question this file
	// exists to answer. The cost is that the file is as sensitive as whatever
	// passes through it, which is why it is 0600.
	const command = `curl -H "Authorization: Bearer sk-live-SECRET" https://api.example.com`

	var buf bytes.Buffer
	NewWriter(&buf).Command("ws", "/w", command, ".", 0, false, time.Second, "none")

	ev := decode(t, strings.TrimSpace(buf.String()))
	if ev.Command != command {
		t.Errorf("the command was altered:\n got  %q\n want %q", ev.Command, command)
	}
}

func TestDeniedRecordsWhichGuardAndWhy(t *testing.T) {
	// A refusal is the record most worth keeping: it is the one event that says
	// something tried to do what the guards exist to stop.
	var buf bytes.Buffer
	NewWriter(&buf).Denied("ws_1", "bash", "command-denylist", "rm -rf / is refused")

	ev := decode(t, strings.TrimSpace(buf.String()))
	if ev.Kind != KindDenied {
		t.Errorf("Kind = %q", ev.Kind)
	}
	if ev.Tool != "bash" || ev.Guard != "command-denylist" {
		t.Errorf("%+v", ev)
	}
	if !strings.Contains(ev.Reason, "rm -rf /") {
		t.Errorf("Reason = %q", ev.Reason)
	}
}

func TestOpenCreatesAPrivateFileAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	first, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	first.Command("ws_1", "/w", "one", ".", 0, false, time.Second, "none")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600: this file holds commands verbatim", perm)
	}

	// A restart must not truncate the history.
	second, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	second.Command("ws_2", "/w", "two", ".", 0, false, time.Second, "none")
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := lines(string(raw))
	if len(got) != 2 {
		t.Fatalf("got %d records after a restart, want 2:\n%s", len(got), raw)
	}
	if decode(t, got[0]).Command != "one" || decode(t, got[1]).Command != "two" {
		t.Errorf("records are not in order: %v", got)
	}
}

func TestOpenWithNoPathIsOff(t *testing.T) {
	l, err := Open("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Enabled() {
		t.Error("auditing is on with no path configured")
	}
}

func TestOpenReportsAPathItCannotUse(t *testing.T) {
	// Surfaced to the caller rather than swallowed: auditing was asked for
	// explicitly, and silently running without it is the one outcome nobody
	// would want.
	_, err := Open(filepath.Join(t.TempDir(), "no", "such", "dir", "a.jsonl"), nil)
	if err == nil {
		t.Fatal("an unusable audit path was accepted")
	}
}

// failingWriter fails every write, as a full disk would.
type failingWriter struct{ writes int }

func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	return 0, errors.New("no space left on device")
}

func TestAFailedWriteDoesNotFailTheRequest(t *testing.T) {
	// Otherwise a full disk becomes an outage, and anyone who wanted the audit
	// trail switched off would have a way to switch it off.
	w := &failingWriter{}
	l := NewWriter(w)

	var reported int
	l.onError = func(error) { reported++ }

	for range 5 {
		l.Command("ws", "/w", "ls", ".", 0, false, time.Second, "none")
	}

	if w.writes != 5 {
		t.Errorf("stopped trying after a failure: %d writes", w.writes)
	}
	if reported != 1 {
		t.Errorf("reported the same failure %d times; once is enough", reported)
	}
}

func TestConcurrentWritesProduceWholeLines(t *testing.T) {
	// Tool calls run in parallel, and a torn record is worse than a missing
	// one: it cannot be parsed and it makes every later line suspect.
	var buf bytes.Buffer
	l := NewWriter(&buf)

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 25 {
				l.Command("ws", "/w",
					strings.Repeat("x", 200)+string(rune('a'+i))+string(rune('0'+j%10)),
					".", 0, false, time.Second, "none")
			}
		}(i)
	}
	wg.Wait()

	got := lines(buf.String())
	if len(got) != 400 {
		t.Fatalf("got %d records, want 400", len(got))
	}
	for i, line := range got {
		if !json.Valid([]byte(line)) {
			t.Fatalf("record %d is torn: %q", i, line)
		}
	}
}
