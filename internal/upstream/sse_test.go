package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prakhargurunani/harness-api/internal/oai"
)

// Transcript-driven tests.
//
// Every fixture under testdata/sse is replayed through the production path —
// Decoder, Stream.Recv, StreamAccumulator — and the assembled result is compared
// against what the transcript should mean. See testdata/sse/README.md for the
// provenance of these files: they are hand-written from documented wire formats,
// not captured from live providers.

const transcriptDir = "../../testdata/sse"

// replay runs a transcript file through the full streaming path.
func replay(t *testing.T, name string) (*oai.StreamAccumulator, error) {
	t.Helper()

	f, err := os.Open(filepath.Join(transcriptDir, name))
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}

	stream := NewStream(f, FormatAuto)
	defer stream.Close()

	acc := &oai.StreamAccumulator{}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return acc, nil
		}
		if err != nil {
			return acc, err
		}
		acc.Add(chunk)
	}
}

func mustReplay(t *testing.T, name string) *oai.StreamAccumulator {
	t.Helper()
	acc, err := replay(t, name)
	if err != nil {
		t.Fatalf("replay %s: %v", name, err)
	}
	return acc
}

func TestTranscriptContentOnly(t *testing.T) {
	tests := []struct {
		file         string
		wantContent  string
		wantFinish   string
		whatItProves string
	}{
		{
			file: "vllm_no_done_no_space.sse", wantContent: "Hello there", wantFinish: "stop",
			whatItProves: "no space after data: and no [DONE] sentinel",
		},
		{
			file: "ndjson_plain.ndjson", wantContent: "Line delimited", wantFinish: "stop",
			whatItProves: "newline-delimited JSON with no SSE framing",
		},
		{
			file: "keepalive_comments.sse", wantContent: "first second", wantFinish: "stop",
			whatItProves: "comment keepalives skipped; multi-line data: rejoined",
		},
		{
			file: "crlf_line_endings.sse", wantContent: "CRLF", wantFinish: "stop",
			whatItProves: "CRLF terminators leave no stray \\r on payloads",
		},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			acc := mustReplay(t, tt.file)

			if got := acc.Message().Content.String(); got != tt.wantContent {
				t.Errorf("content = %q, want %q (%s)", got, tt.wantContent, tt.whatItProves)
			}
			if acc.FinishReason == nil || *acc.FinishReason != tt.wantFinish {
				t.Errorf("finish_reason = %v, want %q", acc.FinishReason, tt.wantFinish)
			}
		})
	}
}

func TestTranscriptParallelToolCalls(t *testing.T) {
	acc := mustReplay(t, "openai_parallel_tools.sse")
	calls := acc.Message().ToolCalls

	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2: %+v", len(calls), calls)
	}

	// Fragments for the two indices interleave in the transcript. Getting these
	// right is the whole point: array position is not identity, index is.
	if calls[0].ID != "call_read_1" || calls[0].Function.Name != "read" {
		t.Errorf("call 0 = %+v", calls[0])
	}
	if got := calls[0].Function.Arguments; got != `{"file_path":"/main.go"}` {
		t.Errorf("call 0 arguments = %q, want the fragments concatenated in order", got)
	}
	if calls[1].ID != "call_grep_2" || calls[1].Function.Name != "grep" {
		t.Errorf("call 1 = %+v", calls[1])
	}
	if got := calls[1].Function.Arguments; got != `{"pattern":"TODO"}` {
		t.Errorf("call 1 arguments = %q", got)
	}

	// An assistant message that only calls tools has no text, and null is the
	// neutral encoding before the quirk layer adjusts it per provider.
	if acc.Message().Content.Kind != oai.ContentNull {
		t.Errorf("content kind = %v, want null for a tools-only message", acc.Message().Content.Kind)
	}
}

func TestTranscriptIndexlessToolCalls(t *testing.T) {
	// The failure this guards against: defaulting every indexless fragment to slot
	// 0, which concatenates two unrelated argument strings into JSON garbage.
	acc := mustReplay(t, "indexless_tool_calls.sse")
	calls := acc.Message().ToolCalls

	if len(calls) != 2 {
		t.Fatalf("got %d tool calls, want 2 distinct calls: %+v", len(calls), calls)
	}

	byID := map[string]oai.ToolCall{}
	for _, c := range calls {
		byID[c.ID] = c
	}

	alpha, ok := byID["call_alpha"]
	if !ok {
		t.Fatalf("call_alpha missing; got %+v", calls)
	}
	if alpha.Function.Name != "read" || alpha.Function.Arguments != `{"file_path":"/a.go"}` {
		t.Errorf("call_alpha = %+v", alpha)
	}

	beta, ok := byID["call_beta"]
	if !ok {
		t.Fatalf("call_beta missing; got %+v", calls)
	}
	if beta.Function.Name != "glob" || beta.Function.Arguments != `{"pattern":"**/*.go"}` {
		t.Errorf("call_beta = %+v", beta)
	}
}

func TestTranscriptSplitEscapes(t *testing.T) {
	acc := mustReplay(t, "split_escapes.sse")
	calls := acc.Message().ToolCalls

	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}

	// The accumulated value is the raw JSON *source*, still carrying its escapes —
	// é has not become é and \n has not become a newline. That only happens
	// when it is finally unmarshalled, below.
	// Built by concatenation so the six-character escape \u00e9 is unambiguous
	// in this source file rather than being normalised to a literal e-acute.
	want := `{"text":"a\"b\\c","note":"caf` + `\u00e9` + ` \n done"}`
	if got := calls[0].Function.Arguments; got != want {
		t.Fatalf("arguments = %q\nwant        %q", got, want)
	}

	// The real assertion: the concatenated result is valid JSON with the intended
	// values. Fragments were split mid-escape and mid-\u sequence, so anything
	// that parsed or trimmed them along the way produces garbage here.
	var args struct {
		Text string `json:"text"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("reassembled arguments are not valid JSON: %v", err)
	}
	if args.Text != `a"b\c` {
		t.Errorf("text = %q, want %q", args.Text, `a"b\c`)
	}
	if args.Note != "café \n done" {
		t.Errorf("note = %q, want %q", args.Note, "café \n done")
	}
}

func TestTranscriptSingleChunkToolCall(t *testing.T) {
	// A provider that buffers server-side must produce the same result as one that
	// fragments, so the accumulator cannot assume more than one fragment arrives.
	acc := mustReplay(t, "single_chunk_tool.sse")
	calls := acc.Message().ToolCalls

	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].Function.Name != "bash" || calls[0].Function.Arguments != `{"command":"go test ./..."}` {
		t.Errorf("call = %+v", calls[0])
	}
}

func TestTranscriptEmptyArguments(t *testing.T) {
	acc := mustReplay(t, "empty_args_tool.sse")
	calls := acc.Message().ToolCalls

	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	// "" is not valid JSON; everything downstream unmarshals this.
	if got := calls[0].Function.Arguments; got != "{}" {
		t.Errorf("arguments = %q, want %q for a zero-argument call", got, "{}")
	}
	var into map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &into); err != nil {
		t.Errorf("normalised arguments must unmarshal: %v", err)
	}
}

func TestTranscriptReasoningSeparateFromContent(t *testing.T) {
	acc := mustReplay(t, "deepseek_reasoning.sse")
	msg := acc.Message()

	if got := msg.Content.String(); got != "The answer is 4." {
		t.Errorf("content = %q", got)
	}
	// Concatenating reasoning into content would corrupt the answer, and echoing
	// it upstream is a 400 on DeepSeek. It has to stay separable.
	if got := msg.ReasoningContent; got != "The user asks for a sum. 2 + 2 = 4." {
		t.Errorf("reasoning = %q", got)
	}
	if strings.Contains(msg.Content.String(), "2 + 2") {
		t.Error("reasoning leaked into content")
	}
}

func TestTranscriptUsageOnFinalChunk(t *testing.T) {
	// The final chunk has usage and an EMPTY choices array. Indexing choices[0]
	// unconditionally panics here.
	acc := mustReplay(t, "usage_final_chunk.sse")

	if acc.Usage == nil {
		t.Fatal("usage from the trailing chunk was lost")
	}
	if acc.Usage.TotalTokens != 13 {
		t.Errorf("total_tokens = %d, want 13", acc.Usage.TotalTokens)
	}
	if got := acc.Message().Content.String(); got != "hi" {
		t.Errorf("content = %q, want %q", got, "hi")
	}
	// The earlier real finish_reason must survive the later null one.
	if acc.FinishReason == nil || *acc.FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", acc.FinishReason)
	}
}

func TestTranscriptErrorMidStream(t *testing.T) {
	// HTTP 200, content already flowing, then a failure. Reporting this as
	// "malformed chunk" would hide the real cause.
	_, err := replay(t, "error_mid_stream.sse")
	if err == nil {
		t.Fatal("expected an error from a mid-stream error object")
	}
	if !strings.Contains(err.Error(), "maximum context length exceeded") {
		t.Errorf("error should carry the provider's message, got: %v", err)
	}
	var upErr *Error
	if !errors.As(err, &upErr) {
		t.Errorf("error should be an *upstream.Error, got %T", err)
	}
}

func TestTranscriptResponseShape(t *testing.T) {
	// The agent loop consumes streamed and non-streamed responses through one code
	// path, so an assembled stream must look exactly like a normal completion.
	acc := mustReplay(t, "openai_parallel_tools.sse")
	resp := acc.Response()

	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion (not .chunk)", resp.Object)
	}
	if resp.ID != "chatcmpl-p1" || resp.Model != "gpt-4o" {
		t.Errorf("identity fields lost: %+v", resp)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(resp.Choices))
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != oai.FinishToolCalls {
		t.Errorf("finish_reason = %v", resp.Choices[0].FinishReason)
	}
}

func TestEveryTranscriptIsExercised(t *testing.T) {
	// A fixture nobody replays is worse than no fixture: it looks like coverage.
	entries, err := os.ReadDir(transcriptDir)
	if err != nil {
		t.Fatalf("read transcript dir: %v", err)
	}

	exercised := map[string]bool{
		"openai_parallel_tools.sse": true,
		"deepseek_reasoning.sse":    true,
		"vllm_no_done_no_space.sse": true,
		"ndjson_plain.ndjson":       true,
		"keepalive_comments.sse":    true,
		"indexless_tool_calls.sse":  true,
		"split_escapes.sse":         true,
		"usage_final_chunk.sse":     true,
		"single_chunk_tool.sse":     true,
		"error_mid_stream.sse":      true,
		"crlf_line_endings.sse":     true,
		"empty_args_tool.sse":       true,
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "README.md" {
			continue
		}
		if !exercised[name] {
			t.Errorf("transcript %s is never replayed by a test; add it or delete it", name)
		}
	}
}

func TestDecoderRejectsOversizedEvent(t *testing.T) {
	// An unbounded event is an easy way to exhaust memory on a malformed stream.
	huge := "data: " + strings.Repeat("x", maxEventBytes+1) + "\n\n"
	dec := NewDecoder(strings.NewReader(huge), FormatSSE)

	_, err := dec.Next()
	if !errors.Is(err, ErrEventTooLarge) {
		t.Errorf("err = %v, want ErrEventTooLarge", err)
	}
}

func TestDecoderHandlesFinalEventWithoutTrailingBlankLine(t *testing.T) {
	// A stream cut off after the last data: line, with no blank separator, still
	// has a complete final event.
	input := `data: {"id":"a"}`
	dec := NewDecoder(strings.NewReader(input), FormatSSE)

	payload, err := dec.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(payload) != `{"id":"a"}` {
		t.Errorf("payload = %q", payload)
	}
	if _, err := dec.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("second Next = %v, want io.EOF", err)
	}
}

func TestSplitField(t *testing.T) {
	tests := []struct{ line, field, value string }{
		{"data: {}", "data", "{}"},
		{"data:{}", "data", "{}"},
		{"data:  leading", "data", " leading"}, // only ONE space is framing
		{"event: message", "event", "message"},
		{"bare", "bare", ""},
	}
	for _, tt := range tests {
		f, v := splitField([]byte(tt.line))
		if f != tt.field || v != tt.value {
			t.Errorf("splitField(%q) = (%q, %q), want (%q, %q)", tt.line, f, v, tt.field, tt.value)
		}
	}
}
