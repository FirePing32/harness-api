package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/FirePing32/harness-api/internal/workspace"
)

// The edit tool decides more of an agent's task success than anything else in
// this package, and almost all of that is in what it says when a match fails.
//
// The matching itself is deliberately unforgiving: exact bytes, no whitespace
// normalisation, no fuzzy fallback, no regex. A fuzzy matcher looks like it
// helps and is far worse, because the failure mode changes from "the edit did
// not apply" to "the edit applied somewhere the model did not intend", and
// nothing downstream catches that.
//
// The cost of being strict is paid entirely in the error path. A model whose
// edit fails has one question — what does the file actually contain there? —
// and an error that answers it turns a five-attempt indentation fight into one
// retry. So a failed match locates the nearest similar text, prints it with
// real whitespace and real line numbers, and when the difference is only
// leading whitespace it says so outright, because tabs and spaces are invisible
// in a conversation and that is the single most common cause.

// Edit replaces an exact string in a file.
type Edit struct{}

// NewEdit builds the edit tool.
func NewEdit() *Edit { return &Edit{} }

func (*Edit) Name() string { return "edit" }

func (*Edit) Description() string {
	return "Replace an exact piece of text in a file.\n\n" +
		"Read the file first. old_string must match the file byte for byte, " +
		"including indentation — copy it from the output of read rather than " +
		"retyping it. Do not include the line numbers that read adds.\n\n" +
		"old_string must appear exactly once, so include enough surrounding " +
		"context to make it unique. To change every occurrence instead, set " +
		"replace_all.\n\n" +
		"To create a new file, use write."
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

func (*Edit) Parameters(SchemaDialect) json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file, relative to the workspace root."
    },
    "old_string": {
      "type": "string",
      "description": "The exact text to replace, copied byte for byte from the file."
    },
    "new_string": {
      "type": "string",
      "description": "The text to put in its place."
    },
    "replace_all": {
      "type": "boolean",
      "description": "Replace every occurrence instead of requiring exactly one."
    }
  },
  "required": ["path", "old_string", "new_string"],
  "additionalProperties": false
}`)
}

// ConcurrencySafe is false, and unconditionally so. An edit is a
// read-modify-write against a file another call might also be touching, and it
// invalidates ledger state. It runs alone.
func (*Edit) ConcurrencySafe(json.RawMessage) bool { return false }

// EditResult is the structured outcome of an edit.
type EditResult struct {
	Path         string `json:"path"`
	DisplayPath  string `json:"display_path"`
	Replacements int    `json:"replacements"`
	FirstLine    int    `json:"first_line"`
	// Snippet shows the changed region so the model can confirm the result
	// without spending another read on it.
	Snippet View `json:"snippet"`
}

func (*Edit) Execute(_ context.Context, s *workspace.Session, raw json.RawMessage) (any, error) {
	var args editArgs
	if err := parseArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Path == "" {
		return nil, Errorf(CodeInvalidArgs, "path is required.")
	}
	if args.OldString == "" {
		return nil, Errorf(CodeInvalidArgs, "old_string is empty.").
			WithHint("Give the exact text to replace. To create a file or replace its " +
				"whole contents, use write.")
	}
	// A no-op edit is always a mistake, and usually means the model believes it
	// has changed something. Saying so is better than reporting success.
	if args.OldString == args.NewString {
		return nil, Errorf(CodeInvalidArgs,
			"old_string and new_string are identical, so this edit would change nothing.").
			WithHint("Check that new_string contains the change you intended.")
	}

	j := s.Jail()
	rel, err := j.Rel(args.Path)
	if err != nil {
		return nil, err
	}

	content, err := j.ReadFile(args.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.Ledger().ObserveAbsent(rel, s.Turn())
			return nil, Errorf(CodeNotFound, "%s does not exist.", j.Display(rel)).
				WithHint("Use write to create it, or glob to find the right path.")
		}
		return nil, Errorf(CodeIO, "could not read %s: %s", j.Display(rel), err)
	}

	// Read-before-edit, checked against the current bytes rather than a flag, so
	// a file rewritten by something else since the read is rejected here.
	if err := s.Ledger().Authorize(rel, content, true); err != nil {
		return nil, err
	}

	text := string(content)
	count := strings.Count(text, args.OldString)

	switch {
	case count == 0:
		return nil, noMatchError(j.Display(rel), text, args.OldString)
	case count > 1 && !args.ReplaceAll:
		return nil, ambiguousMatchError(j.Display(rel), text, args.OldString, count)
	}

	var updated string
	if args.ReplaceAll {
		updated = strings.ReplaceAll(text, args.OldString, args.NewString)
	} else {
		updated = strings.Replace(text, args.OldString, args.NewString, 1)
	}

	info, err := j.Stat(args.Path)
	perm := fs.FileMode(0o644)
	if err == nil {
		perm = info.Mode().Perm()
	}
	if err := j.WriteFile(args.Path, []byte(updated), perm); err != nil {
		return nil, Errorf(CodeIO, "could not write %s: %s", j.Display(rel), err)
	}

	// The file is now the observed version. Without this the model would have to
	// re-read before every follow-up edit, which is a round trip per change.
	newContent := []byte(updated)
	var modTime time.Time
	if info, err := j.Stat(args.Path); err == nil {
		modTime = info.ModTime()
	}
	s.Ledger().Observe(rel, newContent, modTime, s.Turn())

	firstLine := lineOf(text, strings.Index(text, args.OldString))
	return &EditResult{
		Path:         rel,
		DisplayPath:  j.Display(rel),
		Replacements: count,
		FirstLine:    firstLine,
		Snippet:      snippetAround(newContent, firstLine, strings.Count(args.NewString, "\n")+1),
	}, nil
}

func (*Edit) Render(_ json.RawMessage, result any) string {
	r, ok := result.(*EditResult)
	if !ok {
		return ""
	}

	var b strings.Builder
	if r.Replacements == 1 {
		fmt.Fprintf(&b, "Edited %s at line %d.\n\n", r.DisplayPath, r.FirstLine)
	} else {
		fmt.Fprintf(&b, "Edited %s, replacing %d occurrences. The first is at line %d.\n\n",
			r.DisplayPath, r.Replacements, r.FirstLine)
	}
	// Showing the result back saves a confirming read, which the model would
	// otherwise spend a whole turn on.
	b.WriteString(r.Snippet.RenderPlain())
	return b.String()
}

// noMatchError explains a failed match by showing what the file actually holds.
func noMatchError(displayPath, text, old string) *Error {
	e := Errorf(CodeNoMatch, "no text in %s matches old_string exactly.", displayPath)

	oldLines := strings.Split(old, "\n")
	fileLines := splitLines([]byte(text))
	firstOld := strings.TrimSpace(oldLines[0])

	// The strongest signal: a line that is identical once indentation is
	// ignored. That means the content is right and only whitespace differs,
	// which is invisible in a transcript and therefore very hard to self-correct.
	var wsCandidates []int
	for i, line := range fileLines {
		if firstOld != "" && strings.TrimSpace(line) == firstOld {
			wsCandidates = append(wsCandidates, i)
		}
	}

	if len(wsCandidates) > 0 {
		i := wsCandidates[0]
		actual := leadingWhitespace(fileLines[i])
		wanted := leadingWhitespace(oldLines[0])
		e.Hint = fmt.Sprintf(
			"Line %d has the same text but different leading whitespace: the file has %s, "+
				"old_string has %s. Copy the line from read output rather than retyping it.\n\n%s",
			i+1, describeWhitespace(actual), describeWhitespace(wanted),
			numberedWindow(fileLines, i, len(oldLines)))
		return e
	}

	// Otherwise fall back to the most similar line, which usually means the text
	// has moved on or was paraphrased from memory.
	if best, score := closestLine(fileLines, firstOld); best >= 0 && score > 0.5 {
		e.Hint = fmt.Sprintf(
			"The closest text is at line %d. Read the file again if it has changed since "+
				"you last saw it.\n\n%s",
			best+1, numberedWindow(fileLines, best, len(oldLines)))
		return e
	}

	e.Hint = "Read the file to see its current contents, then copy the text to replace " +
		"directly from that output."
	return e
}

// ambiguousMatchError names the line numbers, so the retry is targeted.
func ambiguousMatchError(displayPath, text, old string, count int) *Error {
	lines := occurrenceLines(text, old)

	var where strings.Builder
	for i, n := range lines {
		if i > 0 {
			where.WriteString(", ")
		}
		if i == 6 {
			fmt.Fprintf(&where, "and %d more", len(lines)-6)
			break
		}
		fmt.Fprintf(&where, "%d", n)
	}

	return Errorf(CodeAmbiguous,
		"old_string appears %d times in %s (lines %s), so it is ambiguous which one to change.",
		count, displayPath, where.String()).
		WithHint("Add surrounding lines to old_string until it identifies one place uniquely, " +
			"or set replace_all to change every occurrence.")
}

// occurrenceLines returns the 1-based line of each occurrence of sub in text.
func occurrenceLines(text, sub string) []int {
	var out []int
	offset := 0
	for {
		i := strings.Index(text[offset:], sub)
		if i < 0 {
			return out
		}
		out = append(out, lineOf(text, offset+i))
		offset += i + len(sub)
	}
}

// lineOf converts a byte offset to a 1-based line number.
func lineOf(text string, offset int) int {
	if offset < 0 {
		return 0
	}
	return strings.Count(text[:offset], "\n") + 1
}

// closestLine finds the file line most similar to want, with a 0..1 score.
func closestLine(lines []string, want string) (int, float64) {
	if want == "" {
		return -1, 0
	}
	best, bestScore := -1, 0.0
	for i, line := range lines {
		if score := similarity(strings.TrimSpace(line), want); score > bestScore {
			best, bestScore = i, score
		}
	}
	return best, bestScore
}

// similarity scores two strings by their shared prefix and suffix. It is not a
// real edit distance, and does not need to be: the only job is picking which
// line to show a model that already knows what it was looking for.
func similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}

	maxLen := max(len(a), len(b))
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	return float64(prefix+suffix) / float64(maxLen)
}

// numberedWindow renders a few lines around index i, in read's format so the
// model can copy straight out of it.
func numberedWindow(lines []string, i, span int) string {
	const context = 2
	if span < 1 {
		span = 1
	}

	start := max(i-context, 0)
	end := min(i+span+context, len(lines))

	var b strings.Builder
	for n := start; n < end; n++ {
		line := lines[n]
		if len(line) > 200 {
			line = clipRunes(line, 200) + lineTruncationMarker
		}
		fmt.Fprintf(&b, "%6d\t%s\n", n+1, line)
	}
	return strings.TrimRight(b.String(), "\n")
}

// leadingWhitespace returns the indentation of a line.
func leadingWhitespace(s string) string {
	return s[:len(s)-len(strings.TrimLeft(s, " \t"))]
}

// describeWhitespace names an indentation in words, because the characters
// themselves are indistinguishable once they are in a conversation.
func describeWhitespace(ws string) string {
	tabs := strings.Count(ws, "\t")
	spaces := strings.Count(ws, " ")

	switch {
	case tabs == 0 && spaces == 0:
		return "none"
	case tabs > 0 && spaces > 0:
		return fmt.Sprintf("%d tab(s) and %d space(s)", tabs, spaces)
	case tabs > 0:
		return fmt.Sprintf("%d tab(s)", tabs)
	default:
		return fmt.Sprintf("%d space(s)", spaces)
	}
}

// snippetAround builds a small view centred on the edited region.
func snippetAround(content []byte, firstLine, span int) View {
	const context = 3
	offset := max(firstLine-context, 1)
	return BuildView(content, offset, span+2*context)
}
