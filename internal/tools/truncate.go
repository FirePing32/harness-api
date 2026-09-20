package tools

import (
	"bytes"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Truncation is asymmetric, on purpose.
//
// A file view truncates the *end*: the model asked to see a file starting
// somewhere, and the useful part is where it started. The footer tells it how
// to get the rest.
//
// Shell output truncates the *beginning*. The interesting part of a failed
// build is the error at the bottom, under a hundred kilobytes of compilation
// progress. Head-truncating a build log reliably discards the one thing the
// model needs and leaves it certain the command succeeded.
//
// A single uniform truncator would have to pick one, and would be wrong half
// the time.

// File-view limits, matching the DeepSeek Harness defaults.
const (
	// MaxLines is the default line budget for one read.
	MaxLines = 2000

	// MaxLineChars caps a single line. Minified bundles and embedded base64 are
	// single lines of megabytes; without this one line exhausts the budget.
	MaxLineChars = 2000

	// MaxViewBytes caps the rendered view regardless of line count.
	MaxViewBytes = 50 << 10
)

// LineTruncation marks a truncated line so the model knows the tail is missing
// rather than assuming the file really ends there.
const lineTruncationMarker = "… [line truncated]"

// View is a paginated, line-numbered look at a file.
type View struct {
	// Lines are the rendered lines, already clipped to MaxLineChars.
	Lines []string `json:"lines"`
	// Offset is the 1-based line number of Lines[0].
	Offset int `json:"offset"`
	// TotalLines is the file's full line count.
	TotalLines int `json:"total_lines"`
	// Truncated reports that lines were withheld after the last one shown.
	Truncated bool `json:"truncated"`
	// LongLines counts lines clipped to MaxLineChars.
	LongLines int `json:"long_lines,omitempty"`
	// ByteLimited reports that the byte cap stopped the view before the line cap.
	ByteLimited bool `json:"byte_limited,omitempty"`
}

// NextOffset is the offset that continues the view, or 0 if it is complete.
func (v View) NextOffset() int {
	if !v.Truncated {
		return 0
	}
	return v.Offset + len(v.Lines)
}

// BuildView slices content into a line-numbered view starting at the 1-based
// line offset. limit <= 0 means MaxLines.
func BuildView(content []byte, offset, limit int) View {
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 || limit > MaxLines {
		limit = MaxLines
	}

	all := splitLines(content)
	v := View{Offset: offset, TotalLines: len(all)}

	if offset > len(all) {
		return v
	}

	budget := MaxViewBytes
	for i := offset - 1; i < len(all); i++ {
		if len(v.Lines) >= limit {
			v.Truncated = true
			break
		}

		line := all[i]
		if n := utf8.RuneCountInString(line); n > MaxLineChars {
			line = clipRunes(line, MaxLineChars) + lineTruncationMarker
			v.LongLines++
		}

		// Always take the first line even if it alone blows the budget: a view
		// with no lines at all looks like an empty file.
		if len(v.Lines) > 0 && len(line) > budget {
			v.Truncated = true
			v.ByteLimited = true
			break
		}
		budget -= len(line)
		v.Lines = append(v.Lines, line)
	}

	if offset-1+len(v.Lines) < len(all) {
		v.Truncated = true
	}
	return v
}

// Render writes the view in cat -n form.
//
// The line numbers are not decoration. The edit tool matches exact text, and
// when a match fails or is ambiguous the recovery is to name a line number.
// A model that was never shown line numbers cannot do that, and falls back to
// guessing at the surrounding text — which is the expensive failure mode this
// whole design is arranged to avoid.
func (v View) Render(displayPath string) string {
	if v.TotalLines == 0 {
		// Never return the empty string. A model reads "" as a failed call and
		// retries, often several times, before concluding the file is empty.
		return fmt.Sprintf("%s is empty (0 bytes).", displayPath)
	}
	if len(v.Lines) == 0 {
		return fmt.Sprintf("%s has %d lines; there is nothing at line %d.",
			displayPath, v.TotalLines, v.Offset)
	}

	var b strings.Builder
	for i, line := range v.Lines {
		fmt.Fprintf(&b, "%6d\t%s\n", v.Offset+i, line)
	}

	if footer := v.footer(displayPath); footer != "" {
		b.WriteString("\n")
		b.WriteString(footer)
		b.WriteString("\n")
	}
	return b.String()
}

// RenderPlain writes the numbered lines with no footer and no empty-file
// prose. Used where the view is already inside a message that has explained
// what it is, such as the snippet an edit returns.
func (v View) RenderPlain() string {
	var b strings.Builder
	for i, line := range v.Lines {
		fmt.Fprintf(&b, "%6d\t%s\n", v.Offset+i, line)
	}
	return b.String()
}

// footer explains what was withheld and how to reach it. Stating the remaining
// count and the exact next offset turns "show me the rest" from a guess into a
// single correct call.
func (v View) footer(displayPath string) string {
	var notes []string

	if v.LongLines > 0 {
		notes = append(notes, fmt.Sprintf(
			"%d line(s) longer than %d characters were shortened.", v.LongLines, MaxLineChars))
	}

	if v.Truncated {
		shown := v.Offset + len(v.Lines) - 1
		remaining := v.TotalLines - shown
		reason := ""
		if v.ByteLimited {
			reason = " (the output size limit was reached first)"
		}
		notes = append(notes, fmt.Sprintf(
			"Showing lines %d-%d of %d%s. %d lines were not shown; continue with offset=%d, "+
				"or search first with grep to find the part you need.",
			v.Offset, shown, v.TotalLines, reason, remaining, v.NextOffset()))
	}

	return strings.Join(notes, " ")
}

// splitLines splits content into lines without keeping the terminators. A
// trailing newline does not create a final empty line, which is what every
// editor shows and what the model expects when it counts lines.
func splitLines(content []byte) []string {
	if len(content) == 0 {
		return nil
	}
	s := string(content)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "\n")
}

func clipRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// TailLimit caps captured command output.
const TailLimit = 30 << 10

// TruncateTail keeps the last limit bytes, cutting at a line boundary so the
// first surviving line is whole. It returns the text and how many bytes were
// dropped.
func TruncateTail(b []byte, limit int) (string, int) {
	if limit <= 0 || len(b) <= limit {
		return string(b), 0
	}

	cut := len(b) - limit
	if i := bytes.IndexByte(b[cut:], '\n'); i >= 0 && i < limit/2 {
		cut += i + 1
	}
	return string(b[cut:]), cut
}

// IsBinary reports whether content looks like something a language model
// should not be shown. A NUL byte is the test every tool uses, and it is
// right far more often than any heuristic worth the code.
func IsBinary(content []byte) bool {
	const probe = 8 << 10
	if len(content) > probe {
		content = content[:probe]
	}
	return bytes.IndexByte(content, 0) >= 0
}

// HumanBytes formats a size for a tool message.
func HumanBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
