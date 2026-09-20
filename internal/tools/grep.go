package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/FirePing32/harness-api/internal/workspace"
	"github.com/bmatcuk/doublestar/v4"
)

// Grep limits.
const (
	grepDefaultLimit = 100
	grepMaxLimit     = 500
	// grepMaxFileBytes skips files too large to be worth scanning line by line.
	// A file this size is generated, vendored, or data; none of them are what
	// the model is looking for.
	grepMaxFileBytes = 8 << 20
	// grepMaxScan bounds how many files are opened.
	grepMaxScan = 50_000
)

// Grep searches file contents by regular expression.
type Grep struct{}

// NewGrep builds the grep tool.
func NewGrep() *Grep { return &Grep{} }

func (*Grep) Name() string { return "grep" }

func (*Grep) Description() string {
	return "Search file contents in the workspace with a regular expression.\n\n" +
		"Syntax is RE2, the same as Go and ripgrep. Character classes, anchors, " +
		"groups, alternation and repetition all work. Lookahead (?=), lookbehind (?<=) " +
		"and backreferences (\\1) do not exist in RE2 and will be rejected — express " +
		"the intent with a plain pattern instead.\n\n" +
		"Results are grouped by file, most recently modified first, and each match " +
		"carries its line number so you can read or edit that region directly. " +
		"Files excluded by .gitignore and binary files are not searched.\n\n" +
		"Prefer this over reading whole files when you are looking for something " +
		"specific. Use glob to find files by name."
}

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path,omitempty"`
	Glob            string `json:"glob,omitempty"`
	CaseInsensitive bool   `json:"case_insensitive,omitempty"`
	FilesOnly       bool   `json:"files_only,omitempty"`
	Limit           int    `json:"limit,omitempty"`
}

func (*Grep) Parameters(SchemaDialect) json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "RE2 regular expression to search for."
    },
    "path": {
      "type": "string",
      "description": "Directory to search in, relative to the workspace root. Defaults to the whole workspace."
    },
    "glob": {
      "type": "string",
      "description": "Only search files whose path matches this glob, such as \"**/*.go\"."
    },
    "case_insensitive": {
      "type": "boolean",
      "description": "Match without regard to case."
    },
    "files_only": {
      "type": "boolean",
      "description": "List only the names of files containing a match, not the matching lines."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of matching lines to return. Defaults to ` + itoa(grepDefaultLimit) + `."
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)
}

// ConcurrencySafe: searching only reads.
func (*Grep) ConcurrencySafe(json.RawMessage) bool { return true }

// GrepMatch is one matching line.
type GrepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// GrepResult is the structured outcome of a search.
type GrepResult struct {
	Pattern   string      `json:"pattern"`
	Root      string      `json:"root"`
	Matches   []GrepMatch `json:"matches,omitempty"`
	Files     []string    `json:"files,omitempty"`
	Total     int         `json:"total"`
	FileCount int         `json:"file_count"`
	Truncated bool        `json:"truncated,omitempty"`
	FilesOnly bool        `json:"files_only,omitempty"`
	Scanned   int         `json:"scanned"`
	Skipped   int         `json:"skipped,omitempty"`
}

func (*Grep) Execute(ctx context.Context, s *workspace.Session, raw json.RawMessage) (any, error) {
	var args grepArgs
	if err := parseArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Pattern == "" {
		return nil, Errorf(CodeInvalidArgs, "pattern is required.").
			WithHint("For example \"func NewServer\" or \"TODO|FIXME\".")
	}

	expr := args.Pattern
	if args.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, unsupportedRegexpError(args.Pattern, err)
	}

	var fileGlob string
	if args.Glob != "" {
		fileGlob = filepathToSlash(args.Glob)
		if !doublestar.ValidatePattern(fileGlob) {
			return nil, Errorf(CodeInvalidArgs, "%q is not a valid glob pattern.", args.Glob)
		}
	}

	j := s.Jail()
	start := "."
	if args.Path != "" {
		rel, err := j.Rel(args.Path)
		if err != nil {
			return nil, err
		}
		info, err := j.Stat(args.Path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, Errorf(CodeNotFound, "%s does not exist.", j.Display(rel))
			}
			return nil, Errorf(CodeIO, "could not search %s: %s", j.Display(rel), err)
		}
		if !info.IsDir() {
			// Searching one named file is a reasonable thing to ask for, so allow
			// it rather than making the model restructure the call.
			return grepSingleFile(s, re, rel, args)
		}
		start = filepathToSlash(rel)
	}

	limit := args.Limit
	if limit <= 0 {
		limit = grepDefaultLimit
	}
	if limit > grepMaxLimit {
		limit = grepMaxLimit
	}

	res := &GrepResult{Pattern: args.Pattern, Root: j.Display(start), FilesOnly: args.FilesOnly}
	ignore := s.Ignore()

	type hit struct {
		path    string
		modTime time.Time
		matches []GrepMatch
	}
	var hits []hit

	walkErr := fs.WalkDir(j.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p == start {
			return nil
		}

		if ignore.Match(p, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if fileGlob != "" {
			rel := p
			if start != "." {
				rel = strings.TrimPrefix(strings.TrimPrefix(p, start), "/")
			}
			if ok, _ := doublestar.Match(fileGlob, rel); !ok {
				return nil
			}
		}

		res.Scanned++
		if res.Scanned > grepMaxScan {
			return errScanLimit
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > grepMaxFileBytes {
			res.Skipped++
			return nil
		}

		content, err := j.ReadFile(p)
		if err != nil {
			return nil
		}
		if IsBinary(content) {
			res.Skipped++
			return nil
		}

		found := matchLines(re, content, p)
		if len(found) == 0 {
			return nil
		}
		hits = append(hits, hit{path: p, modTime: info.ModTime(), matches: found})
		return nil
	})

	if walkErr != nil && !errors.Is(walkErr, errScanLimit) {
		if ctx.Err() != nil {
			return nil, walkErr
		}
		return nil, Errorf(CodeIO, "search failed: %s", walkErr)
	}

	// Most recently modified file first, for the same reason glob does it.
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].modTime.Equal(hits[b].modTime) {
			return hits[a].path < hits[b].path
		}
		return hits[a].modTime.After(hits[b].modTime)
	})

	res.FileCount = len(hits)
	for _, h := range hits {
		res.Total += len(h.matches)
		res.Files = append(res.Files, h.path)
		if args.FilesOnly {
			continue
		}
		for _, m := range h.matches {
			if len(res.Matches) >= limit {
				res.Truncated = true
				continue
			}
			res.Matches = append(res.Matches, m)
		}
	}
	if args.FilesOnly && len(res.Files) > limit {
		res.Files = res.Files[:limit]
		res.Truncated = true
	}

	return res, nil
}

// grepSingleFile handles a path argument that names a file rather than a directory.
func grepSingleFile(s *workspace.Session, re *regexp.Regexp, rel string, args grepArgs) (any, error) {
	j := s.Jail()
	content, err := j.ReadFile(rel)
	if err != nil {
		return nil, Errorf(CodeIO, "could not read %s: %s", j.Display(rel), err)
	}
	res := &GrepResult{
		Pattern: args.Pattern, Root: j.Display(rel),
		FilesOnly: args.FilesOnly, Scanned: 1,
	}
	if IsBinary(content) {
		res.Skipped = 1
		return res, nil
	}

	res.Matches = matchLines(re, content, rel)
	res.Total = len(res.Matches)
	if res.Total > 0 {
		res.FileCount = 1
		res.Files = []string{rel}
	}
	return res, nil
}

// matchLines finds the lines of content matching re.
func matchLines(re *regexp.Regexp, content []byte, path string) []GrepMatch {
	var out []GrepMatch
	for i, line := range splitLines(content) {
		if !re.MatchString(line) {
			continue
		}
		// Clip here as well as in file views: a match inside a minified bundle
		// would otherwise return one line of several hundred kilobytes.
		if len(line) > MaxLineChars {
			line = clipRunes(line, MaxLineChars) + lineTruncationMarker
		}
		out = append(out, GrepMatch{Path: path, Line: i + 1, Text: line})
	}
	return out
}

// unsupportedRegexpError translates a compile failure into something actionable.
//
// Models reach for PCRE constructs constantly, because that is what most
// languages give them. Go's own message for a lookahead is "invalid or
// unsupported Perl syntax", which does not say which construct or what to do.
func unsupportedRegexpError(pattern string, err error) *Error {
	msg := err.Error()
	e := Errorf(CodeInvalidArgs, "%q is not a valid RE2 pattern: %s", pattern, trimRegexpError(msg))

	switch {
	case strings.Contains(pattern, "(?=") || strings.Contains(pattern, "(?!"):
		return e.WithHint("RE2 has no lookahead. Match the surrounding text directly, " +
			"or search for the simpler pattern and filter the results yourself.")
	case strings.Contains(pattern, "(?<"):
		return e.WithHint("RE2 has no lookbehind. Include the preceding text in the " +
			"pattern and use a capture group instead.")
	case regexp.MustCompile(`\\[1-9]`).MatchString(pattern):
		return e.WithHint("RE2 has no backreferences. Search for the general shape, " +
			"then check the repeated part yourself.")
	}
	return e.WithHint("RE2 supports character classes, anchors, groups, alternation and " +
		"repetition, but not lookaround or backreferences.")
}

func trimRegexpError(msg string) string {
	return strings.TrimPrefix(msg, "error parsing regexp: ")
}

func (*Grep) Render(_ json.RawMessage, result any) string {
	r, ok := result.(*GrepResult)
	if !ok {
		return ""
	}

	if r.Total == 0 {
		return r.renderEmpty()
	}

	var b strings.Builder

	if r.FilesOnly {
		for _, f := range r.Files {
			b.WriteString(f)
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "\n%d file(s) contain a match, most recently modified first.\n", r.FileCount)
		return b.String()
	}

	// Grouped by file with the name written once. Repeating "path:line:text" on
	// every match, ripgrep-style, spends a lot of context on the same string.
	current := ""
	for _, m := range r.Matches {
		if m.Path != current {
			if current != "" {
				b.WriteString("\n")
			}
			b.WriteString(m.Path)
			b.WriteString("\n")
			current = m.Path
		}
		fmt.Fprintf(&b, "%6d\t%s\n", m.Line, m.Text)
	}

	b.WriteString("\n")
	if r.Truncated {
		fmt.Fprintf(&b, "Showing %d of %d matches in %d file(s). Narrow the pattern, "+
			"restrict it with glob or path, or raise limit to see the rest.\n",
			len(r.Matches), r.Total, r.FileCount)
	} else {
		fmt.Fprintf(&b, "%d match(es) in %d file(s), most recently modified first.\n",
			r.Total, r.FileCount)
	}
	return b.String()
}

func (r *GrepResult) renderEmpty() string {
	var b strings.Builder
	fmt.Fprintf(&b, "No matches for %q", r.Pattern)
	if r.Root != "." {
		fmt.Fprintf(&b, " in %s", r.Root)
	}
	b.WriteString(".")

	if r.Scanned == 0 {
		b.WriteString("\nNo files were searched — check the path and glob arguments.")
	} else {
		fmt.Fprintf(&b, "\nSearched %d file(s).", r.Scanned)
	}
	if r.Skipped > 0 {
		fmt.Fprintf(&b, " %d were skipped as binary or too large.", r.Skipped)
	}
	// Case is the most common reason a search that should have worked did not.
	if hasUpper(r.Pattern) {
		b.WriteString("\nIf case is not significant here, retry with case_insensitive.")
	}
	return b.String()
}

func hasUpper(s string) bool {
	return strings.ToLower(s) != s
}
