package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FirePing32/harness-api/internal/workspace"
)

// touch sets a file's modification time, so ordering tests do not depend on
// how fast the test machine writes files.
func touch(t *testing.T, s *workspace.Session, rel string, at time.Time) {
	t.Helper()
	full := filepath.Join(s.Root(), filepath.FromSlash(rel))
	if err := os.Chtimes(full, at, at); err != nil {
		t.Fatal(err)
	}
}

func globPaths(t *testing.T, out any) []string {
	t.Helper()
	r, ok := out.(*GlobResult)
	if !ok {
		t.Fatalf("result is %T, want *GlobResult", out)
	}
	paths := make([]string, 0, len(r.Matches))
	for _, m := range r.Matches {
		paths = append(paths, m.Path)
	}
	return paths
}

func TestGlobMatchesAtDepthWithGlobstar(t *testing.T) {
	s := newSession(t, map[string]string{
		"main.go":             "package main\n",
		"src/app.go":          "package src\n",
		"src/deep/util.go":    "package deep\n",
		"src/readme.md":       "docs\n",
		"vendor/lib/thing.go": "package lib\n",
	})

	_, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	got := globPaths(t, out)
	if len(got) != 4 {
		t.Fatalf("got %d matches %v, want 4", len(got), got)
	}
}

func TestGlobSingleStarDoesNotCrossDirectories(t *testing.T) {
	s := newSession(t, map[string]string{
		"main.go":    "x\n",
		"src/app.go": "x\n",
	})

	_, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	got := globPaths(t, out)
	if len(got) != 1 || got[0] != "main.go" {
		t.Errorf("got %v, want only main.go — * must not cross a directory boundary", got)
	}
}

func TestGlobOrdersByModificationTimeDescending(t *testing.T) {
	// A working tree's most recently touched file is almost always the one being
	// asked about. Alphabetical order buries it.
	s := newSession(t, map[string]string{
		"a_oldest.go": "x\n",
		"m_middle.go": "x\n",
		"z_newest.go": "x\n",
	})
	now := time.Now()
	touch(t, s, "a_oldest.go", now.Add(-72*time.Hour))
	touch(t, s, "m_middle.go", now.Add(-24*time.Hour))
	touch(t, s, "z_newest.go", now)

	text, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"z_newest.go", "m_middle.go", "a_oldest.go"}
	got := globPaths(t, out)
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	// The model has to be told the order means something.
	if !strings.Contains(text, "most recently modified first") {
		t.Errorf("the footer does not explain the ordering:\n%s", text)
	}
}

func TestGlobHonoursGitignore(t *testing.T) {
	// Without this, a glob in a JavaScript repository returns vendored bundles
	// before any of the project's own code.
	s := newSession(t, map[string]string{
		".gitignore":                "node_modules/\n*.log\n",
		"app.js":                    "x\n",
		"debug.log":                 "x\n",
		"node_modules/lib/index.js": "x\n",
	})

	_, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "**/*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range globPaths(t, out) {
		if strings.HasPrefix(p, "node_modules") || strings.HasSuffix(p, ".log") {
			t.Errorf("%q should have been excluded by .gitignore", p)
		}
	}
}

func TestGlobNeverDescendsIntoAnIgnoredDirectory(t *testing.T) {
	// Pruning, not filtering. Filtering afterwards still walks every file in
	// node_modules, which is where all the time goes.
	s := newSession(t, map[string]string{
		".gitignore":    "skipme/\n",
		"keep.go":       "x\n",
		"skipme/a.go":   "x\n",
		"skipme/b/c.go": "x\n",
		"skipme/b/d.go": "x\n",
	})

	_, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*GlobResult)

	// The ignored directory itself is scanned once; nothing beneath it is.
	if r.Scanned > 3 {
		t.Errorf("Scanned = %d — the walk descended into the ignored directory", r.Scanned)
	}
	if len(r.Matches) != 1 {
		t.Errorf("matches = %v, want only keep.go", globPaths(t, out))
	}
}

func TestGlobAlwaysExcludesDotGit(t *testing.T) {
	s := newSession(t, map[string]string{
		"main.go":              "x\n",
		".git/config":          "x\n",
		".git/objects/ab/cdef": "x\n",
	})

	_, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "**/*"})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range globPaths(t, out) {
		if strings.HasPrefix(p, ".git") {
			t.Errorf("%q leaked from the git directory", p)
		}
	}
}

func TestGlobScopedToSubdirectory(t *testing.T) {
	s := newSession(t, map[string]string{
		"top.go":           "x\n",
		"src/app.go":       "x\n",
		"src/deep/util.go": "x\n",
	})

	_, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "**/*.go", "path": "src"})
	if err != nil {
		t.Fatal(err)
	}
	got := globPaths(t, out)
	if len(got) != 2 {
		t.Fatalf("got %v, want the two files under src", got)
	}
	for _, p := range got {
		if !strings.HasPrefix(p, "src/") {
			t.Errorf("%q is outside the requested path", p)
		}
	}
}

func TestGlobEmptyResultExplainsTheGlobstarMistake(t *testing.T) {
	// Writing "*.go" and expecting recursion is the single most common glob
	// error. "No matches" on its own produces an identical retry.
	s := newSession(t, map[string]string{"src/deep/app.go": "x\n"})

	text, _, err := run(t, NewGlob(), s, map[string]any{"pattern": "*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "**/*.go") {
		t.Errorf("the empty result does not suggest the recursive form:\n%s", text)
	}
}

func TestGlobEmptyResultMentionsIgnoredPaths(t *testing.T) {
	// Otherwise a model that knows the file exists concludes the tool is broken.
	s := newSession(t, map[string]string{
		".gitignore": "*.go\n",
		"main.go":    "x\n",
	})

	text, _, err := run(t, NewGlob(), s, map[string]any{"pattern": "**/*.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, ".gitignore") {
		t.Errorf("the empty result does not mention that paths were excluded:\n%s", text)
	}
}

func TestGlobMarksDirectories(t *testing.T) {
	s := newSession(t, map[string]string{"src/app.go": "x\n"})

	text, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "src"})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*GlobResult)
	if len(r.Matches) != 1 || !r.Matches[0].IsDir {
		t.Fatalf("matches = %+v, want one directory", r.Matches)
	}
	if !strings.Contains(text, "src/") {
		t.Errorf("a directory is not marked with a trailing slash:\n%s", text)
	}
}

func TestGlobRespectsLimitAndReportsTheTotal(t *testing.T) {
	files := make(map[string]string, 20)
	for i := range 20 {
		files[string(rune('a'+i))+".go"] = "x\n"
	}
	s := newSession(t, files)

	text, out, err := run(t, NewGlob(), s, map[string]any{"pattern": "*.go", "limit": 5})
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*GlobResult)

	if len(r.Matches) != 5 {
		t.Errorf("returned %d matches, want the requested 5", len(r.Matches))
	}
	if r.Total != 20 {
		t.Errorf("Total = %d, want the full count of 20", r.Total)
	}
	if !r.Truncated {
		t.Error("Truncated should be set")
	}
	if !strings.Contains(text, "of 20") {
		t.Errorf("the footer hides how many were left out:\n%s", text)
	}
}

func TestGlobRejectsInvalidPattern(t *testing.T) {
	s := newSession(t, nil)

	_, _, err := run(t, NewGlob(), s, map[string]any{"pattern": "[unclosed"})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
	if te.Hint == "" {
		t.Error("the error gives no example of a valid pattern")
	}
}

func TestGlobRejectsMissingPattern(t *testing.T) {
	s := newSession(t, nil)

	_, err := NewGlob().Execute(context.Background(), s, json.RawMessage(`{}`))
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeInvalidArgs {
		t.Fatalf("err = %v, want INVALID_ARGUMENTS", err)
	}
}

func TestGlobRejectsEscapingPath(t *testing.T) {
	s := newSession(t, nil)

	_, _, err := run(t, NewGlob(), s, map[string]any{"pattern": "*", "path": "../.."})
	var pe *workspace.PathError
	if !errorAs(err, &pe) {
		t.Fatalf("err = %v, want a workspace.PathError", err)
	}
}

func TestGlobRejectsFileAsSearchPath(t *testing.T) {
	s := newSession(t, map[string]string{"main.go": "x\n"})

	_, _, err := run(t, NewGlob(), s, map[string]any{"pattern": "*", "path": "main.go"})
	var te *Error
	if !errorAs(err, &te) || te.Code != CodeIsDirectory {
		t.Fatalf("err = %v, want IS_DIRECTORY", err)
	}
}

func TestGlobHonoursContextCancellation(t *testing.T) {
	files := make(map[string]string, 200)
	for i := range 200 {
		files[filepath.Join("d", itoa(i)+".go")] = "x\n"
	}
	s := newSession(t, files)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := NewGlob().Execute(ctx, s, json.RawMessage(`{"pattern":"**/*.go"}`))
	if err == nil {
		t.Fatal("a cancelled search returned successfully")
	}
}

func TestGlobIsConcurrencySafe(t *testing.T) {
	if !NewGlob().ConcurrencySafe(nil) {
		t.Error("a directory walk only reads and should be concurrency-safe")
	}
}
