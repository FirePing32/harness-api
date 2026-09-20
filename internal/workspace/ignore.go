package workspace

import (
	"bufio"
	"io/fs"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Ignore rules, because without them the listing tools drown.
//
// A glob for "**/*.js" in a JavaScript repository returns node_modules before
// it returns any of the project's own code, and the model spends its context
// window reading vendored minified bundles. Honouring .gitignore is what makes
// the file tools usable; it is not a nicety.
//
// This is a deliberate subset of gitignore semantics: patterns, negation,
// directory-only rules, anchoring, and the rule that a negation cannot rescue
// a file inside an ignored directory. Not supported: nested .gitignore files
// below the root, $GIT_DIR/info/exclude, the global core.excludesFile, and
// escaped characters. Those matter to git; for deciding which files to show a
// model they are noise.

// Ignore matches workspace-relative paths against ignore rules.
// The zero value matches nothing and is safe to use.
type Ignore struct {
	rules []ignoreRule
}

type ignoreRule struct {
	// pattern is a doublestar pattern, slash-separated, relative to the root.
	pattern string
	negate  bool
	dirOnly bool
}

// alwaysIgnored applies regardless of .gitignore.
//
// .git is not merely noise — letting an agent read or rewrite object files is
// a way to corrupt a repository through what looks like an ordinary edit.
//
// .harness holds this server's own overflow files. Excluding it keeps a
// workspace-wide grep from matching the output of a command the agent ran
// minutes ago, which reads as a real result and is not one. The directory is
// still reachable by path, because reading a spilled build log is the whole
// reason it was written.
var alwaysIgnored = []string{".git/", ".harness/"}

// LoadIgnore reads the workspace's root .gitignore, if present. A missing or
// unreadable file is not an error: it only means fewer rules.
func LoadIgnore(j *Jail) *Ignore {
	lines := append([]string(nil), alwaysIgnored...)

	if f, err := j.Open(".gitignore"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
	}

	return CompileIgnore(lines)
}

// CompileIgnore builds a matcher from .gitignore-format lines.
func CompileIgnore(lines []string) *Ignore {
	ig := &Ignore{}
	for _, line := range lines {
		if r, ok := compileRule(line); ok {
			ig.rules = append(ig.rules, r)
		}
	}
	return ig
}

func compileRule(line string) (ignoreRule, bool) {
	// Trailing whitespace is not part of a pattern.
	line = strings.TrimRight(line, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}

	var r ignoreRule
	if strings.HasPrefix(line, "!") {
		r.negate = true
		line = line[1:]
	}
	if line == "" {
		return ignoreRule{}, false
	}

	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}

	// A slash anywhere but the end anchors the pattern to the root. Without one,
	// the pattern matches a name at any depth: "*.log" means every .log file,
	// while "build/*.log" means only those in the top-level build directory.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return ignoreRule{}, false
	}

	if anchored {
		r.pattern = line
	} else {
		r.pattern = "**/" + line
	}
	return r, true
}

// Match reports whether a workspace-relative path is ignored.
func (ig *Ignore) Match(p string, isDir bool) bool {
	if ig == nil || len(ig.rules) == 0 {
		return false
	}

	p = strings.Trim(path.Clean(strings.ReplaceAll(p, "\\", "/")), "/")
	if p == "" || p == "." {
		return false
	}

	// Ancestors are checked first. Git excludes everything beneath an ignored
	// directory and does not descend into it, so a negation lower down cannot
	// bring a file back — "node_modules/" followed by "!node_modules/keep.js"
	// still ignores keep.js. Matching only the full path would get this backwards.
	parts := strings.Split(p, "/")
	for i := 1; i < len(parts); i++ {
		if ig.matchExact(strings.Join(parts[:i], "/"), true) {
			return true
		}
	}
	return ig.matchExact(p, isDir)
}

// matchExact applies the rules to one path, last match winning.
func (ig *Ignore) matchExact(p string, isDir bool) bool {
	ignored := false
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		if ok, err := doublestar.Match(r.pattern, p); err == nil && ok {
			ignored = !r.negate
		}
	}
	return ignored
}

// MatchDirEntry is the form the directory walkers want.
func (ig *Ignore) MatchDirEntry(p string, d fs.DirEntry) bool {
	return ig.Match(p, d.IsDir())
}
