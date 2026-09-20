package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FirePing32/harness-api/internal/oai"
)

// sseFrames reads a streaming response into its decoded chunks, plus the raw
// text so comments and the terminator can be asserted on.
type sseFrames struct {
	chunks []oai.ChatCompletionChunk
	errors []map[string]any
	raw    string
	sawDMV bool // saw the [DONE] marker
}

func readSSE(t *testing.T, body io.Reader) sseFrames {
	t.Helper()

	var out sseFrames
	var rawBuilder strings.Builder
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	for sc.Scan() {
		line := sc.Text()
		rawBuilder.WriteString(line)
		rawBuilder.WriteString("\n")

		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			out.sawDMV = true
			continue
		}

		// An error frame and a chunk are told apart by the error key, exactly
		// as the upstream reader does it.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			t.Fatalf("unparseable SSE payload %q: %v", payload, err)
		}
		if _, isErr := probe["error"]; isErr {
			var e map[string]any
			json.Unmarshal([]byte(payload), &e)
			out.errors = append(out.errors, e)
			continue
		}

		var chunk oai.ChatCompletionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("unparseable chunk %q: %v", payload, err)
		}
		out.chunks = append(out.chunks, chunk)
	}
	out.raw = rawBuilder.String()
	return out
}

// content concatenates every content delta, as a client would.
func (f sseFrames) content() string {
	var b strings.Builder
	for _, c := range f.chunks {
		for _, ch := range c.Choices {
			if ch.Delta.Content != nil {
				b.WriteString(*ch.Delta.Content)
			}
		}
	}
	return b.String()
}

// harnessEvents returns every harness progress event in the stream.
func (f sseFrames) harnessEvents(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, c := range f.chunks {
		for _, ch := range c.Choices {
			if len(ch.Delta.Harness) == 0 {
				continue
			}
			var ev map[string]any
			if err := json.Unmarshal(ch.Delta.Harness, &ev); err != nil {
				t.Fatalf("unparseable harness event: %v", err)
			}
			out = append(out, ev)
		}
	}
	return out
}

func (f sseFrames) finishReason() string {
	for _, c := range f.chunks {
		for _, ch := range c.Choices {
			if ch.FinishReason != nil {
				return *ch.FinishReason
			}
		}
	}
	return ""
}

func postStream(t *testing.T, url, body string) sseFrames {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	return readSSE(t, resp.Body)
}

func TestStreamDeliversTheAnswerAsDeltas(t *testing.T) {
	front, _ := newAgentServer(t, nil, []string{
		`{"role":"assistant","content":"The answer is forty-two, and here is a longer sentence so that it has to be split across several delta frames rather than arriving all at once."}`,
	})

	frames := postStream(t, front+"/v1/chat/completions",
		`{"model":"stub","messages":[{"role":"user","content":"hi"}],"stream":true}`)

	if !frames.sawDMV {
		t.Error("the stream did not end with [DONE]")
	}
	if !strings.Contains(frames.content(), "forty-two") {
		t.Errorf("content = %q", frames.content())
	}
	if len(frames.chunks) < 3 {
		t.Errorf("got %d chunks; a long answer should arrive as several deltas", len(frames.chunks))
	}
	if frames.finishReason() != oai.FinishStop {
		t.Errorf("finish_reason = %q, want stop", frames.finishReason())
	}

	// The first chunk opens the message with a role, as OpenAI does.
	if frames.chunks[0].Choices[0].Delta.Role != oai.RoleAssistant {
		t.Error("the stream does not open with a role delta")
	}
}

func TestStreamSuppressesIntermediateTurns(t *testing.T) {
	// The reason tier 1 exists. Concatenating every turn's content produces a
	// transcript of the model thinking out loud, not a reply.
	front, dir := newAgentServer(t, map[string]string{"a.txt": "one\n"}, []string{
		`{"role":"assistant","content":"Let me check that file first.","tool_calls":[` +
			`{"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"a.txt\"}"}}]}`,
		`{"role":"assistant","content":"The file says one."}`,
	})

	frames := postStream(t, front+"/v1/chat/completions",
		`{"model":"stub","messages":[{"role":"user","content":"what is in a.txt"}],`+
			`"stream":true,"harness":{"workspace":"`+dir+`"}}`)

	got := frames.content()
	if strings.Contains(got, "Let me check that file first") {
		t.Errorf("an intermediate turn's preamble reached the client: %q", got)
	}
	if !strings.Contains(got, "The file says one") {
		t.Errorf("the final answer is missing: %q", got)
	}
}

func TestStreamEventsAreOptIn(t *testing.T) {
	front, dir := newAgentServer(t, map[string]string{"a.txt": "one\n"}, []string{
		toolTurn("c1", "read", `{"path":"a.txt"}`),
		`{"role":"assistant","content":"done"}`,
	})

	body := `{"model":"stub","messages":[{"role":"user","content":"read it"}],` +
		`"stream":true,"harness":{"workspace":"` + dir + `"}}`

	if evs := postStream(t, front+"/v1/chat/completions", body).harnessEvents(t); len(evs) != 0 {
		t.Errorf("got %d harness events without asking for them", len(evs))
	}
}

func TestStreamEventsReportToolActivity(t *testing.T) {
	front, dir := newAgentServer(t, map[string]string{"a.txt": "one\n"}, []string{
		toolTurn("c1", "read", `{"path":"a.txt"}`),
		`{"role":"assistant","content":"done"}`,
	})

	frames := postStream(t, front+"/v1/chat/completions",
		`{"model":"stub","messages":[{"role":"user","content":"read it"}],`+
			`"stream":true,"harness":{"workspace":"`+dir+`","stream_events":true}}`)

	events := frames.harnessEvents(t)
	if len(events) == 0 {
		t.Fatal("stream_events was requested but no events arrived")
	}

	var sawStart, sawEnd, sawDone bool
	for _, ev := range events {
		switch ev["type"] {
		case "tool_start":
			sawStart = true
			if ev["tool"] != "read" {
				t.Errorf("tool_start names %v, want read", ev["tool"])
			}
			if s, _ := ev["summary"].(string); !strings.Contains(s, "a.txt") {
				t.Errorf("summary %q does not say what is being worked on", s)
			}
		case "tool_end":
			sawEnd = true
			if ev["call_id"] != "c1" {
				t.Errorf("tool_end call_id = %v, want c1", ev["call_id"])
			}
		case "done":
			sawDone = true
		}
	}
	if !sawStart || !sawEnd || !sawDone {
		t.Errorf("missing events: start=%v end=%v done=%v", sawStart, sawEnd, sawDone)
	}

	// The answer must still arrive alongside the events.
	if !strings.Contains(frames.content(), "done") {
		t.Errorf("content = %q", frames.content())
	}
}

func TestStreamEventsRideInsideDelta(t *testing.T) {
	// Not at the top level of the chunk: SDKs model delta as an open map and a
	// chunk as a fixed struct, so an unknown top-level key makes stricter
	// clients reject the whole frame.
	front, dir := newAgentServer(t, map[string]string{"a.txt": "x\n"}, []string{
		toolTurn("c1", "read", `{"path":"a.txt"}`),
		`{"role":"assistant","content":"ok"}`,
	})

	resp, err := http.Post(front+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"stub","messages":[{"role":"user","content":"x"}],`+
			`"stream":true,"harness":{"workspace":"`+dir+`","stream_events":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(raw), `"delta":{"harness":`) {
		t.Errorf("events are not inside delta:\n%s", firstN(string(raw), 600))
	}
}

func TestStreamUsageIsOptInAndAggregated(t *testing.T) {
	// A server per request: the scripted provider replays its turns in order
	// and has no notion of a conversation, so sharing one across two requests
	// would leave the second starting past the end of the script.
	script := []string{
		toolTurn("c1", "read", `{"path":"a.txt"}`),
		`{"role":"assistant","content":"ok"}`,
	}
	request := func(extra string) sseFrames {
		front, dir := newAgentServer(t, map[string]string{"a.txt": "x\n"}, script)
		return postStream(t, front+"/v1/chat/completions",
			`{"model":"stub","messages":[{"role":"user","content":"x"}],"stream":true`+
				extra+`,"harness":{"workspace":"`+dir+`"}}`)
	}

	// Without stream_options there is no usage frame, because the extra chunk
	// breaks clients that assume the stream ends at the finish reason.
	for _, c := range request("").chunks {
		if c.Usage != nil {
			t.Error("usage was sent without stream_options.include_usage")
		}
	}

	var usage *oai.Usage
	for _, c := range request(`,"stream_options":{"include_usage":true}`).chunks {
		if c.Usage != nil {
			usage = c.Usage
		}
	}
	if usage == nil {
		t.Fatal("include_usage was set but no usage frame arrived")
	}
	// Two upstream calls at 15 tokens each: the total for the request, not the
	// final turn's numbers.
	if usage.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want 30 across both calls", usage.TotalTokens)
	}
}

func TestStreamReturnsTheSessionHeader(t *testing.T) {
	// Headers are flushed with the first frame, so this has to be set before
	// anything is written.
	front, _ := newAgentServer(t, nil, []string{`{"role":"assistant","content":"hi"}`})

	resp, err := http.Post(front+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"stub","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.Header.Get(HeaderSession) == "" {
		t.Error("no session id on a streaming response")
	}
}

func TestStreamReportsCeilingsInBandAndAsFinishReason(t *testing.T) {
	var turns []string
	for range 10 {
		turns = append(turns, toolTurn("c", "glob", `{"pattern":"**/*"}`))
	}
	front, dir := newAgentServer(t, map[string]string{"a.txt": "x\n"}, turns)

	frames := postStream(t, front+"/v1/chat/completions",
		`{"model":"stub","messages":[{"role":"user","content":"loop"}],"stream":true,`+
			`"harness":{"workspace":"`+dir+`","max_iterations":2}}`)

	if frames.finishReason() != oai.FinishLength {
		t.Errorf("finish_reason = %q, want length", frames.finishReason())
	}
	if !strings.Contains(frames.content(), "Stopped after") {
		t.Errorf("a truncated stream does not say it was truncated: %q", frames.content())
	}
}

func TestStreamErrorAfterHeadersGoesInBand(t *testing.T) {
	// Once the headers are out there is no way back to an HTTP status code.
	front, _ := newAgentServer(t, nil, nil)

	// An unknown tool in the allowlist fails inside the run, after the stream
	// has already been opened.
	frames := postStream(t, front+"/v1/chat/completions",
		`{"model":"stub","messages":[{"role":"user","content":"hi"}],"stream":true,`+
			`"harness":{"tools":["nonexistent"]}}`)

	if len(frames.errors) == 0 {
		t.Fatalf("no error frame; raw stream:\n%s", firstN(frames.raw, 400))
	}
	e, _ := frames.errors[0]["error"].(map[string]any)
	if msg, _ := e["message"].(string); !strings.Contains(msg, "nonexistent") {
		t.Errorf("error message = %q", msg)
	}
	if !frames.sawDMV {
		t.Error("the stream did not terminate after the error")
	}
}

func TestStreamEditsFilesLikeTheNonStreamingPath(t *testing.T) {
	front, dir := newAgentServer(t, map[string]string{
		"main.go": "package main\n\nfunc oldName() {}\n",
	}, []string{
		toolTurn("c1", "read", `{"path":"main.go"}`),
		toolTurn("c2", "edit", `{"path":"main.go","old_string":"oldName","new_string":"newName"}`),
		`{"role":"assistant","content":"Renamed it."}`,
	})

	frames := postStream(t, front+"/v1/chat/completions",
		`{"model":"stub","messages":[{"role":"user","content":"rename"}],"stream":true,`+
			`"harness":{"workspace":"`+dir+`"}}`)

	if !strings.Contains(frames.content(), "Renamed") {
		t.Errorf("content = %q", frames.content())
	}
	changed, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changed), "func newName()") {
		t.Errorf("the file was not edited: %q", changed)
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
