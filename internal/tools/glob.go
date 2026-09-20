package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/prakhargurunani/harness-api/internal/workspace"
)

// Glob limits.
const (
	// globDefaultLimit is how many matches are returned unless asked otherwise.
	globDefaultLimit = 200
	// globMaxLimit is the ceiling a caller may raise the limit to.
	globMaxLimit = 1000
	// globMaxScan bounds the walk itself. A pattern like "**/*" over a large
	// checkout with no ignore file would otherwise stat hundreds of thousands of
	// entries to produce a list nobody can use.
	globMaxScan = 200_000
)

// Glob finds files by path pattern.
type Glob struct{}

// NewGlob builds the glob tool.
func NewGlob() *Glob { return &Glob{} }

func (*Glob) Name() string { return "glob" }

func (*Glob) Description() string {
	return "Find files in the workspace by path pattern.\n\n" +
		"Patterns: * matches within one path segment, ** matches across segments, " +
		"? matches one character, {a,b} matches alternatives. \"*.go\" finds Go files in " +
		"the top level only; \"**/*.go\" finds them at any depth.\n\n" +
		"Results are ordered by modification time, most recently changed first, so the " +
		"files most likely to be relevant come first. Files excluded by .gitignore are " +
		"not listed. Directories are shown with a trailing slash.\n\n" +
		"Use this to locate files before reading them. To search file contents rather " +
		"than names, use grep."
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

func (*Glob) Parameters(SchemaDialect) json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Glob pattern, such as \"**/*.go\" or \"src/**/test_*.py\"."
    },
    "path": {
      "type": "string",
      "description": "Directory to search in, relative to the workspace root. Defaults to the whole workspace. The pattern is matched relative to this directory."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of results. Defaults to ` + itoa(globDefaultLimit) + `."
    }
  },
  "required": ["pattern"],
  "additionalProperties": false
}`)
}

// ConcurrencySafe: a walk only reads directory entries.
func (*Glob) ConcurrencySafe(json.RawMessage) bool { return true }

// GlobMatch is one matched path.
type GlobMatch struct {
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir,omitempty"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// GlobResult is the structured outcome of a glob.
type GlobResult struct {
	Pattern string      `json:"pattern"`
	Root    string      `json:"root"`
	Matches []GlobMatch `json:"matches"`
	// Total is how many matched before the limit was applied.
	Total int `json:"total"`
	// Truncated reports that Total exceeded the limit.
	Truncated bool `json:"truncated,omitempty"`
	// Scanned is how many entries the walk looked at.
	Scanned int `json:"scanned"`
	// ScanLimited reports that the walk stopped early, so results are partial.
	ScanLimited bool `json:"scan_limited,omitempty"`
	// Ignored counts entries skipped by .gitignore, so the footer can say that
	// an empty result is not necessarily an empty directory.
	Ignored int `json:"ignored,omitempty"`
}

func (*Glob) Execute(ctx context.Context, s *workspace.Session, raw json.RawMessage) (any, error) {
	var args globArgs
	if err := parseArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Pattern) == "" {
		return nil, Errorf(CodeInvalidArgs, "pattern is required.").
			WithHint("For example \"**/*.go\" to find every Go file.")
	}

	pattern := path.Clean(filepathToSlash(args.Pattern))
	if !doublestar.ValidatePattern(pattern) {
		return nil, Errorf(CodeInvalidArgs, "%q is not a valid glob pattern.", args.Pattern).
			WithHint("Check for an unclosed bracket or brace. Valid examples: \"*.go\", " +
				"\"**/*_test.go\", \"src/{a,b}/*.ts\".")
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
			return nil, Errorf(CodeIsDirectory, "%s is a file, not a directory.", j.Display(rel)).
				WithHint("path must name a directory to search in; the pattern selects the files.")
		}
		start = filepathToSlash(rel)
	}

	limit := args.Limit
	if limit <= 0 {
		limit = globDefaultLimit
	}
	if limit > globMaxLimit {
		limit = globMaxLimit
	}

	res := &GlobResult{Pattern: pattern, Root: j.Display(start)}
	ignore := s.Ignore()

	// The walk is hand-rolled rather than handed to doublestar.Glob because the
	// pruning is the point: returning early from a matched path is useless, what
	// matters is never descending into node_modules at all. fs.WalkDir's SkipDir
	// does that; a glob function that walks everything and filters afterwards
	// spends its time in directories whose contents are excluded by definition.
	err := fs.WalkDir(j.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory is not a reason to abandon the search.
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

		res.Scanned++
		if res.Scanned > globMaxScan {
			res.ScanLimited = true
			return errScanLimit
		}

		if ignore.Match(p, d.IsDir()) {
			res.Ignored++
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		// Match the pattern relative to the search root, so that a "path" of
		// "src" plus a pattern of "**/*.go" behaves the way it reads.
		rel := p
		if start != "." {
			rel = strings.TrimPrefix(strings.TrimPrefix(p, start), "/")
		}
		ok, matchErr := doublestar.Match(pattern, rel)
		if matchErr != nil || !ok {
			return nil
		}

		res.Total++
		if len(res.Matches) >= limit {
			res.Truncated = true
			return nil
		}

		m := GlobMatch{Path: p, IsDir: d.IsDir()}
		if info, err := d.Info(); err == nil {
			m.Size = info.Size()
			m.ModTime = info.ModTime()
		}
		res.Matches = append(res.Matches, m)
		return nil
	})

	if err != nil && !errors.Is(err, errScanLimit) {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, Errorf(CodeIO, "search failed: %s", err)
	}

	// Most recently modified first. In a working tree the file someone touched
	// five minutes ago is almost always the one being asked about, and an
	// alphabetical list buries it under whatever starts with "a".
	sort.SliceStable(res.Matches, func(a, b int) bool {
		if res.Matches[a].ModTime.Equal(res.Matches[b].ModTime) {
			return res.Matches[a].Path < res.Matches[b].Path
		}
		return res.Matches[a].ModTime.After(res.Matches[b].ModTime)
	})

	return res, nil
}

var errScanLimit = errors.New("scan limit reached")

func (*Glob) Render(_ json.RawMessage, result any) string {
	r, ok := result.(*GlobResult)
	if !ok {
		return ""
	}

	if len(r.Matches) == 0 {
		return r.renderEmpty()
	}

	var b strings.Builder
	for _, m := range r.Matches {
		b.WriteString(m.Path)
		if m.IsDir {
			b.WriteString("/")
		}
		b.WriteString("\n")
	}

	var notes []string
	if r.Truncated {
		notes = append(notes, fmt.Sprintf(
			"Showing %d of %d matches, most recently modified first. Narrow the pattern, "+
				"or raise limit, to see the rest.", len(r.Matches), r.Total))
	} else {
		notes = append(notes, fmt.Sprintf("%d match(es), most recently modified first.", r.Total))
	}
	if r.ScanLimited {
		notes = append(notes, "The search stopped early because the workspace is very large, "+
			"so this list may be incomplete.")
	}

	b.WriteString("\n")
	b.WriteString(strings.Join(notes, " "))
	b.WriteString("\n")
	return b.String()
}

// renderEmpty explains a zero-result search.
//
// "No matches" on its own is the least useful thing a search can say, and the
// usual next move is a second identical call with a marginally different
// pattern. The two things actually worth saying are that * does not cross
// directory boundaries — far and away the most common mistake — and that
// ignored files were skipped, which is why a file the model knows exists can
// fail to appear.
func (r *GlobResult) renderEmpty() string {
	var b strings.Builder
	fmt.Fprintf(&b, "No files matched %q", r.Pattern)
	if r.Root != "." {
		fmt.Fprintf(&b, " under %s", r.Root)
	}
	b.WriteString(".")

	if !strings.Contains(r.Pattern, "**") && strings.Contains(r.Pattern, "*") {
		fmt.Fprintf(&b, "\n* does not cross directory boundaries. To search every "+
			"subdirectory, use \"**/%s\".", path.Base(r.Pattern))
	}
	if r.Ignored > 0 {
		// Phrased as "were not searched" rather than "were skipped": the ignored
		// paths are whatever the walk passed over, not necessarily anything that
		// would have matched. Implying otherwise sends the model looking for a
		// file the ignore rules had nothing to do with.
		fmt.Fprintf(&b, "\n%d path(s) here are excluded by .gitignore and were not searched.", r.Ignored)
	}
	if r.Scanned == 0 {
		b.WriteString("\nThe directory searched is empty.")
	}
	return b.String()
}

// filepathToSlash normalises a caller-supplied pattern or path to forward
// slashes, which is what fs.FS and doublestar use on every platform.
func filepathToSlash(s string) string { return strings.ReplaceAll(s, "\\", "/") }
