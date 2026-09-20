package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FirePing32/harness-api/internal/workspace"
)

// observe records a read of a file, as the read tool would, so an edit is
// authorised without going through the read tool in every test.
func observe(t *testing.T, s *workspace.Session, rel string) {
	t.Helper()
	content, err := s.Jail().ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Jail().Stat(rel)
	if err != nil {
		t.Fatal(err)
	}
	s.Ledger().Observe(rel, content, info.ModTime(), s.Turn())
}

func fileContent(t *testing.T, s *workspace.Session, rel string) string {
	t.Helper()
	b, err := s.Jail().ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEditReplacesExactText(t *testing.T) {
	s := newSession(t, map[string]string{"main.go": "package main\n\nfunc old() {}\n"})
	observe(t, s, "main.go")

	text, out, err := run(t, NewEdit(), s, map[string]any{
		"path": "main.go", "old_string": "func old()", "new_string": "func renamed()",
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := fileContent(t, s, "main.go"); !strings.Contains(got, "func renamed()") {
		t.Errorf("the file was not changed: %q", got)
	}
	if r := out.(*EditResult); r.FirstLine != 3 {
		t.Errorf("FirstLine = %d, want 3", r.FirstLine)
	}
	// The result shows the changed region, so the model does not spend a turn
	// re-reading to confirm.
	if !strings.Contains(text, "func renamed()") {
		t.Errorf("the result does not show the edited region:\n%s", text)
	}
}

func TestEditRequiresTheFileToHaveBeenRead(t *testing.T) {
	s := newSession(t, map[string]string{"main.go": "package main\n"})

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "main.go", "old_string": "package main", "new_string": "package x",
	})
	var oe *workspace.ObservationError
	if !errorAs(err, &oe) || oe.Code != workspace.CodeNotObserved {
		t.Fatalf("err = %v, want FS_NOT_OBSERVED", err)
	}
	if got := fileContent(t, s, "main.go"); got != "package main\n" {
		t.Errorf("the file was modified despite the refusal: %q", got)
	}
}

func TestEditRejectsAFileChangedSinceItWasRead(t *testing.T) {
	// The case a boolean read-before-edit flag misses: the model read the file,
	// something else rewrote it, and editing against the remembered contents
	// would silently discard that work.
	s := newSession(t, map[string]string{"main.go": "original\n"})
	observe(t, s, "main.go")

	full := filepath.Join(s.Root(), "main.go")
	if err := os.WriteFile(full, []byte("rewritten by a build step\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "main.go", "old_string": "original", "new_string": "changed",
	})
	var oe *workspace.ObservationError
	if !errorAs(err, &oe) || oe.Code != workspace.CodeStale {
		t.Fatalf("err = %v, want FS_STALE_VERSION", err)
	}
	if got := fileContent(t, s, "main.go"); got != "rewritten by a build step\n" {
		t.Errorf("the concurrent change was clobbered: %q", got)
	}
}

func TestEditRejectsIdenticalStrings(t *testing.T) {
	// Always a mistake, and usually the model believes it has changed something.
	s := newSession(t, map[string]string{"a.txt": "hello\n"})
	observe(t, s, "a.txt")

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.txt", "old_string": "hello", "new_string": "hello",
	})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
	if !strings.Contains(te.Error(), "identical") {
		t.Errorf("the error does not explain the problem: %q", te)
	}
}

func TestEditAmbiguousMatchNamesTheLineNumbers(t *testing.T) {
	// "appears 3 times" alone leaves the model guessing at which context to add.
	// The line numbers make the retry targeted.
	s := newSession(t, map[string]string{
		"a.go": "x := 1\ny := 2\nx := 1\nz := 3\nx := 1\n",
	})
	observe(t, s, "a.go")

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.go", "old_string": "x := 1", "new_string": "x := 9",
	})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeAmbiguous {
		t.Fatalf("err = %v, want AMBIGUOUS_MATCH", err)
	}
	msg := te.Error()
	for _, want := range []string{"3 times", "1", "3", "5"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error omits %q:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "replace_all") {
		t.Error("the error does not mention replace_all as an option")
	}
	if got := fileContent(t, s, "a.go"); strings.Contains(got, "x := 9") {
		t.Error("an ambiguous edit was applied anyway")
	}
}

func TestEditReplaceAll(t *testing.T) {
	s := newSession(t, map[string]string{"a.go": "old\nkeep\nold\nold\n"})
	observe(t, s, "a.go")

	text, out, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.go", "old_string": "old", "new_string": "new", "replace_all": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fileContent(t, s, "a.go"); got != "new\nkeep\nnew\nnew\n" {
		t.Errorf("content = %q", got)
	}
	if r := out.(*EditResult); r.Replacements != 3 {
		t.Errorf("Replacements = %d, want 3", r.Replacements)
	}
	if !strings.Contains(text, "3 occurrences") {
		t.Errorf("the result does not say how many were replaced:\n%s", text)
	}
}

func TestEditNoMatchDiagnosesAWhitespaceMismatch(t *testing.T) {
	// The single most common edit failure, and the hardest to self-correct,
	// because tabs and spaces look identical in a conversation.
	s := newSession(t, map[string]string{
		"a.go": "func main() {\n\tfmt.Println(\"hi\")\n}\n",
	})
	observe(t, s, "a.go")

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path":       "a.go",
		"old_string": "    fmt.Println(\"hi\")", // four spaces, file has a tab
		"new_string": "    fmt.Println(\"bye\")",
	})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeNoMatch {
		t.Fatalf("err = %v, want NO_MATCH", err)
	}

	msg := te.Error()
	if !strings.Contains(msg, "tab") || !strings.Contains(msg, "space") {
		t.Errorf("the error does not identify the whitespace difference:\n%s", msg)
	}
	if !strings.Contains(msg, "line 2") && !strings.Contains(msg, "Line 2") {
		t.Errorf("the error does not give the line number:\n%s", msg)
	}
	// It must also show the real bytes, numbered, so the model can copy them.
	if !strings.Contains(msg, "     2\t") {
		t.Errorf("the error does not show the actual line in read format:\n%s", msg)
	}
}

func TestEditNoMatchShowsTheClosestText(t *testing.T) {
	s := newSession(t, map[string]string{
		"a.go": "package main\n\nfunc handleRequest(w http.ResponseWriter) {\n}\n",
	})
	observe(t, s, "a.go")

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path":       "a.go",
		"old_string": "func handleRequest(w http.ResponseWriter, r *http.Request) {",
		"new_string": "func handle(w http.ResponseWriter) {",
	})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeNoMatch {
		t.Fatalf("err = %v, want NO_MATCH", err)
	}
	if !strings.Contains(te.Error(), "handleRequest") {
		t.Errorf("the error does not show the nearest actual text:\n%s", te)
	}
}

func TestEditNoMatchWithNothingSimilarStillAdvises(t *testing.T) {
	s := newSession(t, map[string]string{"a.go": "package main\n"})
	observe(t, s, "a.go")

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.go", "old_string": "zzzzz qqqqq wwwww", "new_string": "x",
	})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeNoMatch {
		t.Fatalf("err = %v, want NO_MATCH", err)
	}
	if te.Hint == "" {
		t.Error("a no-match error with no similar text gives no advice at all")
	}
}

func TestEditUpdatesTheLedgerSoConsecutiveEditsWork(t *testing.T) {
	// Without re-observing after a write, every follow-up edit would need an
	// intervening read — a wasted round trip per change.
	s := newSession(t, map[string]string{"a.go": "one\ntwo\nthree\n"})
	observe(t, s, "a.go")

	if _, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.go", "old_string": "one", "new_string": "1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.go", "old_string": "two", "new_string": "2",
	}); err != nil {
		t.Fatalf("a second consecutive edit was refused: %v", err)
	}

	if got := fileContent(t, s, "a.go"); got != "1\n2\nthree\n" {
		t.Errorf("content = %q", got)
	}
}

func TestEditPreservesFileMode(t *testing.T) {
	s := newSession(t, map[string]string{"run.sh": "#!/bin/sh\necho old\n"})
	full := filepath.Join(s.Root(), "run.sh")
	if err := os.Chmod(full, 0o755); err != nil {
		t.Fatal(err)
	}
	observe(t, s, "run.sh")

	if _, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "run.sh", "old_string": "echo old", "new_string": "echo new",
	}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, want the executable bit preserved", info.Mode().Perm())
	}
}

func TestEditMissingFile(t *testing.T) {
	s := newSession(t, nil)

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "nope.go", "old_string": "a", "new_string": "b",
	})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeNotFound {
		t.Fatalf("err = %v, want NOT_FOUND", err)
	}
	if !strings.Contains(te.Hint, "write") {
		t.Errorf("the hint should point at write for creation: %q", te.Hint)
	}
}

func TestEditRejectsEmptyOldString(t *testing.T) {
	s := newSession(t, map[string]string{"a.go": "x\n"})
	observe(t, s, "a.go")

	_, err := NewEdit().Execute(context.Background(), s,
		json.RawMessage(`{"path":"a.go","old_string":"","new_string":"y"}`))
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
}

func TestEditIsNotConcurrencySafe(t *testing.T) {
	// A read-modify-write that also mutates ledger state must run alone.
	if NewEdit().ConcurrencySafe(nil) {
		t.Error("edit must not opt into concurrent execution")
	}
}

func TestEditDoesNotNormaliseWhitespaceWhenMatching(t *testing.T) {
	// Matching must be exact bytes. A fuzzy matcher turns "the edit did not
	// apply" into "the edit applied somewhere unintended", which nothing catches.
	s := newSession(t, map[string]string{"a.go": "a  b\n"}) // two spaces
	observe(t, s, "a.go")

	_, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.go", "old_string": "a b", "new_string": "c", // one space
	})
	if err == nil {
		t.Fatal("a whitespace-normalised match was accepted")
	}
	if got := fileContent(t, s, "a.go"); got != "a  b\n" {
		t.Errorf("the file was changed by a non-exact match: %q", got)
	}
}
