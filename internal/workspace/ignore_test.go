package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreBasicPatterns(t *testing.T) {
	ig := CompileIgnore([]string{
		"# a comment",
		"",
		"*.log",
		"node_modules/",
		"/build",
		"docs/*.tmp",
	})

	cases := []struct {
		path  string
		isDir bool
		want  bool
		why   string
	}{
		{"app.log", false, true, "unanchored pattern matches at the top level"},
		{"src/deep/app.log", false, true, "unanchored pattern matches at any depth"},
		{"app.log.txt", false, false, "the pattern is anchored to the end"},
		{"node_modules", true, true, "trailing slash matches a directory"},
		{"node_modules", false, false, "trailing slash must not match a file of that name"},
		{"src/node_modules", true, true, "an unanchored directory pattern matches at depth"},
		{"build", true, true, "a leading slash anchors to the root"},
		{"src/build", true, false, "a leading slash must not match at depth"},
		{"docs/notes.tmp", false, true, "an embedded slash anchors the pattern"},
		{"a/docs/notes.tmp", false, false, "an anchored pattern must not match at depth"},
		{"src/main.go", false, false, "an unmatched path is not ignored"},
	}

	for _, c := range cases {
		if got := ig.Match(c.path, c.isDir); got != c.want {
			t.Errorf("Match(%q, dir=%v) = %v, want %v — %s", c.path, c.isDir, got, c.want, c.why)
		}
	}
}

func TestIgnoreNegationRescuesAFile(t *testing.T) {
	ig := CompileIgnore([]string{"*.log", "!keep.log"})

	if !ig.Match("app.log", false) {
		t.Error("app.log should be ignored")
	}
	if ig.Match("keep.log", false) {
		t.Error("keep.log was negated and should not be ignored")
	}
}

func TestIgnoreLastMatchWins(t *testing.T) {
	// Order matters: re-ignoring after a negation takes effect.
	ig := CompileIgnore([]string{"*.log", "!keep.log", "keep.log"})
	if !ig.Match("keep.log", false) {
		t.Error("the later rule should win")
	}
}

func TestIgnoreNegationCannotEscapeAnIgnoredDirectory(t *testing.T) {
	// Real git behaviour, and easy to get backwards: git never descends into an
	// ignored directory, so nothing inside it can be un-ignored. Matching only
	// the full path would wrongly resurrect the file.
	ig := CompileIgnore([]string{"node_modules/", "!node_modules/keep.js"})

	if !ig.Match("node_modules/keep.js", false) {
		t.Error("a negation inside an ignored directory must not rescue the file")
	}
}

func TestIgnoreInheritsFromIgnoredAncestor(t *testing.T) {
	ig := CompileIgnore([]string{"build/"})

	for _, p := range []string{"build/out.js", "build/a/b/c.js"} {
		if !ig.Match(p, false) {
			t.Errorf("%q is under an ignored directory and should be ignored", p)
		}
	}
}

func TestIgnoreGlobstar(t *testing.T) {
	ig := CompileIgnore([]string{"**/generated/**"})

	if !ig.Match("src/generated/api.go", false) {
		t.Error("globstar pattern should match")
	}
	if ig.Match("src/main.go", false) {
		t.Error("globstar pattern matched too much")
	}
}

func TestIgnoreEmptyMatchesNothing(t *testing.T) {
	var ig *Ignore
	if ig.Match("anything", false) {
		t.Error("a nil Ignore must match nothing")
	}
	if CompileIgnore(nil).Match("anything", false) {
		t.Error("an empty Ignore must match nothing")
	}
}

func TestLoadIgnoreAlwaysExcludesGitDirectory(t *testing.T) {
	// Not just noise: letting an agent read or rewrite object files is a way to
	// corrupt a repository through what looks like an ordinary edit.
	dir := t.TempDir()
	j, err := OpenJail(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	ig := LoadIgnore(j)
	if !ig.Match(".git", true) {
		t.Error(".git should be ignored even with no .gitignore present")
	}
	if !ig.Match(".git/objects/ab/cdef", false) {
		t.Error("contents of .git should be ignored")
	}
}

func TestLoadIgnoreReadsWorkspaceFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"),
		[]byte("# deps\nnode_modules/\n*.tmp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJail(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	ig := LoadIgnore(j)
	if !ig.Match("node_modules", true) {
		t.Error("the workspace .gitignore was not applied")
	}
	if !ig.Match("x.tmp", false) {
		t.Error("the workspace .gitignore was not applied")
	}
	if ig.Match("main.go", false) {
		t.Error("an unrelated file was ignored")
	}
}

func TestLoadIgnoreWithNoFileStillWorks(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJail(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()

	ig := LoadIgnore(j)
	if ig.Match("main.go", false) {
		t.Error("a missing .gitignore should not ignore anything beyond the defaults")
	}
}

func TestIgnoreTrailingWhitespaceIsNotPartOfThePattern(t *testing.T) {
	ig := CompileIgnore([]string{"*.log   "})
	if !ig.Match("app.log", false) {
		t.Error("trailing whitespace should be stripped from a pattern")
	}
}
