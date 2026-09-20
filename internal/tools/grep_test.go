package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func grepPaths(t *testing.T, out any) []string {
	t.Helper()
	r, ok := out.(*GrepResult)
	if !ok {
		t.Fatalf("result is %T, want *GrepResult", out)
	}
	return r.Files
}

func TestGrepFindsMatchesWithLineNumbers(t *testing.T) {
	// The line numbers are what make a match actionable: the model can read or
	// edit that region directly instead of pulling the whole file.
	s := newSession(t, map[string]string{
		"main.go": "package main\n\nfunc NewServer() {}\n\nfunc other() {}\n",
	})

	text, out, err := run(t, NewGrep(), s, map[string]any{"pattern": "func New"})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*GrepResult)

	if r.Total != 1 {
		t.Fatalf("Total = %d, want 1", r.Total)
	}
	if r.Matches[0].Line != 3 {
		t.Errorf("Line = %d, want 3", r.Matches[0].Line)
	}
	if !strings.Contains(text, "     3\tfunc NewServer() {}") {
		t.Errorf("the rendered match is not line-numbered:\n%s", text)
	}
}

func TestGrepGroupsByFile(t *testing.T) {
	// Repeating "path:line:text" per match, ripgrep-style, spends a lot of
	// context on the same string.
	s := newSession(t, map[string]string{
		"a.go": "TODO one\nTODO two\n",
		"b.go": "TODO three\n",
	})

	text, _, err := run(t, NewGrep(), s, map[string]any{"pattern": "TODO"})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(text, "a.go"); n != 1 {
		t.Errorf("the file name appears %d times, want once per group:\n%s", n, text)
	}
}

func TestGrepRejectsLookaheadWithAnActionableMessage(t *testing.T) {
	// Models reach for PCRE constantly. Go's own message is "invalid or
	// unsupported Perl syntax", which names neither the construct nor a fix.
	s := newSession(t, map[string]string{"a.go": "x\n"})

	_, _, err := run(t, NewGrep(), s, map[string]any{"pattern": `foo(?=bar)`})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
	if !strings.Contains(te.Hint, "lookahead") {
		t.Errorf("the hint does not name the unsupported construct: %q", te.Hint)
	}
}

func TestGrepRejectsBackreferenceWithAnActionableMessage(t *testing.T) {
	s := newSession(t, map[string]string{"a.go": "x\n"})

	_, _, err := run(t, NewGrep(), s, map[string]any{"pattern": `(\w+)\s+\1`})
	var te *Error
	if !errorAs(err, &te) {
		t.Fatalf("err = %v, want a tool Error", err)
	}
	if !strings.Contains(te.Hint, "backreference") {
		t.Errorf("the hint does not name the unsupported construct: %q", te.Hint)
	}
}

func TestGrepCaseInsensitive(t *testing.T) {
	s := newSession(t, map[string]string{"a.go": "Error handling\n"})

	_, out, err := run(t, NewGrep(), s, map[string]any{
		"pattern": "error", "case_insensitive": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*GrepResult); r.Total != 1 {
		t.Errorf("Total = %d, want 1 with case_insensitive", r.Total)
	}
}

func TestGrepEmptyResultSuggestsCaseInsensitive(t *testing.T) {
	// Case is the most common reason a search that should have worked did not.
	s := newSession(t, map[string]string{"a.go": "error handling\n"})

	text, _, err := run(t, NewGrep(), s, map[string]any{"pattern": "Error"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "case_insensitive") {
		t.Errorf("an empty result with a capitalised pattern does not suggest case_insensitive:\n%s", text)
	}
}

func TestGrepHonoursGitignoreAndSkipsBinary(t *testing.T) {
	s := newSession(t, map[string]string{
		".gitignore":    "vendor/\n",
		"a.go":          "needle\n",
		"vendor/lib.go": "needle\n",
		"blob.bin":      "needle\x00\x01\n",
	})
	s.ReloadIgnore()

	_, out, err := run(t, NewGrep(), s, map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	files := grepPaths(t, out)
	if len(files) != 1 || files[0] != "a.go" {
		t.Errorf("files = %v, want only a.go", files)
	}
	if r := out.(*GrepResult); r.Skipped == 0 {
		t.Error("the binary file was not reported as skipped")
	}
}

func TestGrepFiltersByGlob(t *testing.T) {
	s := newSession(t, map[string]string{
		"a.go": "needle\n",
		"a.md": "needle\n",
	})

	_, out, err := run(t, NewGrep(), s, map[string]any{
		"pattern": "needle", "glob": "**/*.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	files := grepPaths(t, out)
	if len(files) != 1 || files[0] != "a.go" {
		t.Errorf("files = %v, want only a.go", files)
	}
}

func TestGrepFilesOnly(t *testing.T) {
	s := newSession(t, map[string]string{"a.go": "x\nx\nx\n"})

	text, out, err := run(t, NewGrep(), s, map[string]any{
		"pattern": "x", "files_only": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*GrepResult); len(r.Matches) != 0 {
		t.Errorf("files_only still returned %d matching lines", len(r.Matches))
	}
	if !strings.Contains(text, "a.go") {
		t.Errorf("result = %q", text)
	}
}

func TestGrepSearchesASingleNamedFile(t *testing.T) {
	// Passing a file as "path" is a reasonable thing to ask for, and rejecting
	// it would make the model restructure a call that clearly expressed intent.
	s := newSession(t, map[string]string{
		"a.go": "needle\n",
		"b.go": "needle\n",
	})

	_, out, err := run(t, NewGrep(), s, map[string]any{
		"pattern": "needle", "path": "a.go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := out.(*GrepResult); r.Total != 1 {
		t.Errorf("Total = %d, want only the named file's match", r.Total)
	}
}

func TestGrepOrdersFilesByModificationTime(t *testing.T) {
	s := newSession(t, map[string]string{
		"old.go": "needle\n",
		"new.go": "needle\n",
	})
	now := time.Now()
	touch(t, s, "old.go", now.Add(-48*time.Hour))
	touch(t, s, "new.go", now)

	_, out, err := run(t, NewGrep(), s, map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if files := grepPaths(t, out); len(files) != 2 || files[0] != "new.go" {
		t.Errorf("files = %v, want the most recently modified first", files)
	}
}

func TestGrepRespectsLimit(t *testing.T) {
	var sb strings.Builder
	for range 50 {
		sb.WriteString("needle\n")
	}
	s := newSession(t, map[string]string{"a.go": sb.String()})

	text, out, err := run(t, NewGrep(), s, map[string]any{"pattern": "needle", "limit": 5})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*GrepResult)
	if len(r.Matches) != 5 {
		t.Errorf("returned %d matches, want 5", len(r.Matches))
	}
	if r.Total != 50 {
		t.Errorf("Total = %d, want the full count", r.Total)
	}
	if !strings.Contains(text, "of 50") {
		t.Errorf("the footer hides how many were left out:\n%s", text)
	}
}

func TestGrepClipsVeryLongMatchingLines(t *testing.T) {
	// A match inside a minified bundle would otherwise return one line of
	// several hundred kilobytes.
	s := newSession(t, map[string]string{
		"bundle.js": strings.Repeat("a", 5000) + "needle" + strings.Repeat("b", 5000) + "\n",
	})

	_, out, err := run(t, NewGrep(), s, map[string]any{"pattern": "needle"})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*GrepResult)
	if len(r.Matches) != 1 {
		t.Fatalf("Total = %d", r.Total)
	}
	if len(r.Matches[0].Text) > MaxLineChars+len(lineTruncationMarker)+8 {
		t.Errorf("the matching line was not clipped: %d chars", len(r.Matches[0].Text))
	}
}

func TestGrepRequiresPattern(t *testing.T) {
	s := newSession(t, nil)

	_, err := NewGrep().Execute(context.Background(), s, json.RawMessage(`{}`))
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
}

func TestGrepIsConcurrencySafe(t *testing.T) {
	if !NewGrep().ConcurrencySafe(nil) {
		t.Error("searching only reads and should be concurrency-safe")
	}
}
