package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prakhargurunani/harness-api/internal/config"
	"github.com/prakhargurunani/harness-api/internal/workspace"
)

// newSession builds a session over a temp workspace seeded with files.
func newSession(t *testing.T, files map[string]string) *workspace.Session {
	t.Helper()

	m, err := workspace.NewManager(config.Workspace{
		Root:    t.TempDir(),
		IdleTTL: config.Duration(time.Hour),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	s, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}

	for name, content := range files {
		full := filepath.Join(s.Root(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if len(files) > 0 {
		s.ReloadIgnore()
	}
	return s
}

// run executes a tool and returns the rendered text plus the structured result.
func run(t *testing.T, tool Tool, s *workspace.Session, args map[string]any) (string, any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Execute(context.Background(), s, raw)
	if err != nil {
		return "", nil, err
	}
	return tool.Render(raw, out), out, nil
}

func TestReadNumbersLines(t *testing.T) {
	// Line numbers are what later edits refer to; without them a failed match
	// has no cheap recovery.
	s := newSession(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"})

	text, _, err := run(t, NewRead(), s, map[string]any{"path": "main.go"})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"1\tpackage main", "2\t", "3\tfunc main() {}"} {
		if !strings.Contains(text, want) {
			t.Errorf("line %d missing from output:\n%s", i+1, text)
		}
	}
}

func TestReadEmptyFileSaysSoInWords(t *testing.T) {
	// Returning "" makes a model believe the call failed, and it retries —
	// often several times — before concluding the file is empty.
	s := newSession(t, map[string]string{"empty.txt": ""})

	text, _, err := run(t, NewRead(), s, map[string]any{"path": "empty.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("an empty file rendered as an empty string")
	}
	if !strings.Contains(text, "empty") {
		t.Errorf("output does not say the file is empty: %q", text)
	}
}

func TestReadMissingFileRecordsConfirmedAbsence(t *testing.T) {
	// The absence is a real observation: it is what later authorises creating
	// the file without a separate existence-check tool.
	s := newSession(t, nil)

	_, _, err := run(t, NewRead(), s, map[string]any{"path": "new.go"})
	if err == nil {
		t.Fatal("reading a missing file succeeded")
	}
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeNotFound {
		t.Fatalf("err = %v, want NOT_FOUND", err)
	}

	obs, ok := s.Ledger().Lookup("new.go")
	if !ok {
		t.Fatal("no observation was recorded for the missing file")
	}
	if obs.Exists {
		t.Error("the observation claims the file exists")
	}
	if err := s.Ledger().Authorize("new.go", nil, false); err != nil {
		t.Errorf("the recorded absence does not authorise creation: %v", err)
	}
}

func TestReadRecordsFullContentNotJustThePageShown(t *testing.T) {
	// The hash has to cover the whole file. If it covered only the visible page,
	// a change past the end of the view would go undetected and a later edit
	// would silently discard it.
	var sb strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	content := sb.String()
	s := newSession(t, map[string]string{"big.txt": content})

	if _, _, err := run(t, NewRead(), s, map[string]any{"path": "big.txt"}); err != nil {
		t.Fatal(err)
	}

	if err := s.Ledger().Authorize("big.txt", []byte(content), true); err != nil {
		t.Fatalf("the unmodified file was refused: %v", err)
	}
	// Change a line the view never reached.
	changed := strings.Replace(content, "line 2999\n", "line 2999 CHANGED\n", 1)
	if err := s.Ledger().Authorize("big.txt", []byte(changed), true); err == nil {
		t.Error("a change beyond the visible page went undetected")
	}
}

func TestReadPaginates(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= 2500; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	s := newSession(t, map[string]string{"big.txt": sb.String()})

	text, out, err := run(t, NewRead(), s, map[string]any{"path": "big.txt"})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*ReadResult)

	if len(r.View.Lines) != MaxLines {
		t.Errorf("got %d lines, want the %d-line cap", len(r.View.Lines), MaxLines)
	}
	if !r.View.Truncated {
		t.Error("View.Truncated is false on a file longer than the cap")
	}
	if got := r.View.NextOffset(); got != MaxLines+1 {
		t.Errorf("NextOffset = %d, want %d", got, MaxLines+1)
	}
	// The footer has to say how to continue, or the model guesses.
	if !strings.Contains(text, "offset=2001") {
		t.Errorf("footer does not give the next offset:\n%s", lastLines(text, 3))
	}
	if !strings.Contains(text, "grep") {
		t.Errorf("footer does not suggest searching instead of paging:\n%s", lastLines(text, 3))
	}

	text2, out2, err := run(t, NewRead(), s, map[string]any{"path": "big.txt", "offset": 2001})
	if err != nil {
		t.Fatal(err)
	}
	if r2 := out2.(*ReadResult); r2.View.Truncated {
		t.Error("the second page should complete the file")
	}
	if !strings.Contains(text2, "  2001\tline 2001") {
		t.Errorf("the second page does not start where the first stopped:\n%s", firstLines(text2, 2))
	}
}

func TestReadClipsVeryLongLines(t *testing.T) {
	// One minified bundle on a single line would otherwise consume the whole
	// output budget and tell the model nothing.
	long := strings.Repeat("x", 5000)
	s := newSession(t, map[string]string{"bundle.js": "short\n" + long + "\nafter\n"})

	text, out, err := run(t, NewRead(), s, map[string]any{"path": "bundle.js"})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*ReadResult)

	if r.View.LongLines != 1 {
		t.Errorf("LongLines = %d, want 1", r.View.LongLines)
	}
	if !strings.Contains(text, "line truncated") {
		t.Error("a clipped line is not marked, so the model cannot tell it is incomplete")
	}
	if !strings.Contains(text, "     3\tafter") {
		t.Error("lines after the long one were lost")
	}
}

func TestReadRefusesBinary(t *testing.T) {
	s := newSession(t, map[string]string{"a.bin": "PNG\x00\x01\x02binary"})

	_, _, err := run(t, NewRead(), s, map[string]any{"path": "a.bin"})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeBinary {
		t.Fatalf("err = %v, want BINARY_FILE", err)
	}
	if te.Hint == "" {
		t.Error("the binary-file error gives no alternative")
	}
}

func TestReadRefusesDirectoryWithAUsefulHint(t *testing.T) {
	s := newSession(t, map[string]string{"src/main.go": "package main\n"})

	_, _, err := run(t, NewRead(), s, map[string]any{"path": "src"})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeIsDirectory {
		t.Fatalf("err = %v, want IS_DIRECTORY", err)
	}
	if !strings.Contains(te.Hint, "glob") {
		t.Errorf("hint should point at glob: %q", te.Hint)
	}
}

func TestReadRejectsEscapingPath(t *testing.T) {
	s := newSession(t, nil)

	_, _, err := run(t, NewRead(), s, map[string]any{"path": "../../etc/passwd"})
	var pe *workspace.PathError
	if !errorAs(err, &pe) {
		t.Fatalf("err = %v, want a workspace.PathError", err)
	}
	if codeOf(err) != workspace.CodePathOutside {
		t.Errorf("code = %q", codeOf(err))
	}
}

func TestReadRejectsUnknownArgument(t *testing.T) {
	// A model that invents "recursive" and has it silently dropped gets a result
	// that ignores an instruction it believes it gave.
	s := newSession(t, map[string]string{"a.txt": "x\n"})

	_, err := NewRead().Execute(context.Background(), s,
		json.RawMessage(`{"path":"a.txt","recursive":true}`))
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
	if !strings.Contains(te.Error(), "recursive") {
		t.Errorf("the error does not name the unknown key: %q", te)
	}
}

func TestReadArgumentErrorDoesNotLeakGoTypes(t *testing.T) {
	s := newSession(t, map[string]string{"a.txt": "x\n"})

	_, err := NewRead().Execute(context.Background(), s,
		json.RawMessage(`{"path":"a.txt","offset":"first"}`))
	if err == nil {
		t.Fatal("a string offset was accepted")
	}
	msg := err.Error()
	if strings.Contains(msg, "readArgs") || strings.Contains(msg, "Go struct") {
		t.Errorf("the error exposes internal type names: %q", msg)
	}
	if !strings.Contains(msg, "offset") {
		t.Errorf("the error does not name the bad field: %q", msg)
	}
}

func TestReadMissingPathArgument(t *testing.T) {
	s := newSession(t, nil)

	_, err := NewRead().Execute(context.Background(), s, json.RawMessage(`{}`))
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
}

func TestReadOffsetPastEndOfFile(t *testing.T) {
	s := newSession(t, map[string]string{"a.txt": "one\ntwo\n"})

	text, _, err := run(t, NewRead(), s, map[string]any{"path": "a.txt", "offset": 99})
	if err != nil {
		t.Fatalf("an out-of-range offset should not be an error: %v", err)
	}
	if !strings.Contains(text, "2 lines") {
		t.Errorf("the message should say how many lines there are: %q", text)
	}
}

func TestReadIsConcurrencySafe(t *testing.T) {
	if !NewRead().ConcurrencySafe(nil) {
		t.Error("read changes nothing and should be concurrency-safe")
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
