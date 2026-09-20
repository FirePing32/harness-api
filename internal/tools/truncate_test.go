package tools

import (
	"fmt"
	"strings"
	"testing"
)

func TestSplitLinesTrailingNewline(t *testing.T) {
	// A trailing newline must not invent a final empty line. Every editor
	// reports "a\nb\n" as two lines, and the model counts the same way when it
	// later names a line number in an edit.
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"a\n\nb\n", 3},
		{"\n", 1},
	}
	for _, c := range cases {
		if got := len(splitLines([]byte(c.in))); got != c.want {
			t.Errorf("splitLines(%q) = %d lines, want %d", c.in, got, c.want)
		}
	}
}

func TestSplitLinesNormalisesCRLF(t *testing.T) {
	// A carriage return left on the end of every line would be invisible in the
	// rendered view but would break exact-match editing.
	lines := splitLines([]byte("a\r\nb\r\n"))
	for i, l := range lines {
		if strings.Contains(l, "\r") {
			t.Errorf("line %d still carries a carriage return: %q", i, l)
		}
	}
}

func TestBuildViewRespectsLineLimit(t *testing.T) {
	content := manyLines(100)

	v := BuildView([]byte(content), 1, 10)
	if len(v.Lines) != 10 {
		t.Fatalf("got %d lines, want 10", len(v.Lines))
	}
	if !v.Truncated || v.NextOffset() != 11 {
		t.Errorf("Truncated=%v NextOffset=%d, want true and 11", v.Truncated, v.NextOffset())
	}
	if v.TotalLines != 100 {
		t.Errorf("TotalLines = %d, want the full count even when truncated", v.TotalLines)
	}
}

func TestBuildViewCompleteFileIsNotTruncated(t *testing.T) {
	v := BuildView([]byte(manyLines(5)), 1, 10)
	if v.Truncated {
		t.Error("a fully shown file is marked truncated")
	}
	if v.NextOffset() != 0 {
		t.Errorf("NextOffset = %d, want 0 when there is nothing left", v.NextOffset())
	}
}

func TestBuildViewOffsetIsOneBased(t *testing.T) {
	v := BuildView([]byte(manyLines(10)), 3, 2)
	if v.Lines[0] != "line 3" {
		t.Errorf("first line = %q, want line 3 — offsets are 1-based", v.Lines[0])
	}
	if v.Offset != 3 {
		t.Errorf("Offset = %d", v.Offset)
	}
}

func TestBuildViewOffsetBeyondEnd(t *testing.T) {
	v := BuildView([]byte(manyLines(3)), 99, 10)
	if len(v.Lines) != 0 {
		t.Errorf("got %d lines past the end of the file", len(v.Lines))
	}
	if v.TotalLines != 3 {
		t.Errorf("TotalLines = %d", v.TotalLines)
	}
	if got := v.Render("a.txt"); !strings.Contains(got, "nothing at line 99") {
		t.Errorf("render = %q, want it to explain the empty page", got)
	}
}

func TestBuildViewByteCapStopsRunawayOutput(t *testing.T) {
	// Well under the line cap, far over the byte cap: a few hundred long lines.
	var sb strings.Builder
	for range 400 {
		sb.WriteString(strings.Repeat("y", 500))
		sb.WriteString("\n")
	}

	v := BuildView([]byte(sb.String()), 1, MaxLines)
	if !v.ByteLimited {
		t.Fatal("the byte cap did not engage")
	}
	if len(v.Lines) >= 400 {
		t.Error("every line was returned despite the byte cap")
	}
	if !strings.Contains(v.Render("big.txt"), "output size limit") {
		t.Error("the footer does not say why the view stopped early")
	}
}

func TestBuildViewAlwaysReturnsAtLeastOneLine(t *testing.T) {
	// A single line larger than the whole byte budget must still be shown.
	// Returning nothing would be indistinguishable from an empty file.
	huge := strings.Repeat("z", MaxViewBytes*2) + "\n"

	v := BuildView([]byte(huge), 1, MaxLines)
	if len(v.Lines) == 0 {
		t.Fatal("an oversized first line produced an empty view")
	}
}

func TestBuildViewClipsLongLinesAndMarksThem(t *testing.T) {
	content := "short\n" + strings.Repeat("x", MaxLineChars+500) + "\ntail\n"

	v := BuildView([]byte(content), 1, MaxLines)
	if v.LongLines != 1 {
		t.Fatalf("LongLines = %d, want 1", v.LongLines)
	}
	if !strings.Contains(v.Lines[1], "line truncated") {
		t.Error("a clipped line is unmarked, so the model cannot tell it is incomplete")
	}
	if v.Lines[2] != "tail" {
		t.Errorf("lines after the clipped one were lost: %q", v.Lines[2])
	}
	if !strings.Contains(v.Render("f.txt"), "shortened") {
		t.Error("the footer does not mention the shortened lines")
	}
}

func TestClipRunesDoesNotSplitAMultibyteCharacter(t *testing.T) {
	// Cutting mid-rune produces invalid UTF-8, which some providers reject
	// outright and others silently mangle.
	s := strings.Repeat("é", 10)
	got := clipRunes(s, 5)

	if n := len([]rune(got)); n != 5 {
		t.Errorf("clipped to %d runes, want 5", n)
	}
	if !strings.HasPrefix(s, got) {
		t.Errorf("clip produced %q, which is not a prefix of the input", got)
	}
}

func TestViewRenderUsesCatNFormat(t *testing.T) {
	v := BuildView([]byte("alpha\nbeta\n"), 1, 10)
	got := v.Render("f.txt")

	if !strings.Contains(got, "     1\talpha") {
		t.Errorf("line 1 is not in cat -n form:\n%s", got)
	}
	if !strings.Contains(got, "     2\tbeta") {
		t.Errorf("line 2 is not in cat -n form:\n%s", got)
	}
}

func TestViewRenderEmptyFileIsProseNotEmptyString(t *testing.T) {
	got := BuildView(nil, 1, 10).Render("empty.txt")
	if strings.TrimSpace(got) == "" {
		t.Fatal("an empty file rendered as an empty string")
	}
	if !strings.Contains(got, "empty.txt") || !strings.Contains(got, "empty") {
		t.Errorf("render = %q", got)
	}
}

func TestTruncateTailKeepsTheEnd(t *testing.T) {
	// The opposite of a file view, and deliberately so: the error in a failed
	// build is at the bottom, under the compilation progress.
	var sb strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&sb, "compiling module %d\n", i)
	}
	sb.WriteString("FATAL: undefined reference to main\n")

	got, dropped := TruncateTail([]byte(sb.String()), 512)
	if dropped == 0 {
		t.Fatal("nothing was dropped from an oversized output")
	}
	if !strings.Contains(got, "FATAL: undefined reference to main") {
		t.Error("the tail truncator discarded the error at the end of the log")
	}
	if len(got) > 512 {
		t.Errorf("kept %d bytes, over the limit", len(got))
	}
}

func TestTruncateTailStartsAtALineBoundary(t *testing.T) {
	in := []byte(strings.Repeat("aaaaaaaaa\n", 100))

	got, dropped := TruncateTail(in, 95)
	if dropped == 0 {
		t.Fatal("nothing was dropped")
	}
	// Every surviving line must be whole. A partial first line looks like
	// corrupted output rather than a truncation.
	for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if line != "aaaaaaaaa" {
			t.Errorf("line %d = %q, want a complete line", i, line)
		}
	}
}

func TestTruncateTailUnderLimitIsUnchanged(t *testing.T) {
	in := []byte("small output\n")
	got, dropped := TruncateTail(in, 1024)
	if got != string(in) || dropped != 0 {
		t.Errorf("TruncateTail altered output that was already within the limit: %q, %d", got, dropped)
	}
}

func TestIsBinary(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want bool
	}{
		{"plain text", []byte("package main\n"), false},
		{"utf-8 text", []byte("héllo wörld — ok\n"), false},
		{"empty", nil, false},
		{"nul byte", []byte("PNG\x00\x01"), true},
		{"nul late but within the probe", append([]byte(strings.Repeat("a", 4096)), 0), true},
	}
	for _, c := range cases {
		if got := IsBinary(c.in); got != c.want {
			t.Errorf("IsBinary(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:       "0 bytes",
		512:     "512 bytes",
		2048:    "2.0 KB",
		3145728: "3.0 MB",
	}
	for in, want := range cases {
		if got := HumanBytes(in); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func manyLines(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	return sb.String()
}
