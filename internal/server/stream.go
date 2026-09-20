package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/FirePing32/harness-api/internal/agent"
	"github.com/FirePing32/harness-api/internal/oai"
)

// Streaming an agent run is not the same problem as streaming a completion.
//
// A completion has one generation, so its tokens can go straight out. An agent
// run has several, and only the last one is the answer. The others are the
// model saying "Let me check that file" before calling a tool, and
// concatenating them produces something that reads like a transcript of
// someone thinking out loud rather than a reply.
//
// So the default — tier 1 — emits only the final turn's content. That comes
// with a limitation worth stating plainly rather than burying: because the
// final turn is not known to be final until it arrives without tool calls, its
// content is produced before streaming begins. It is then emitted in pieces so
// that progressive renderers behave normally, but there is no time-to-first-
// token benefit over a non-streaming request. What streaming does buy is the
// connection staying open and, with stream_events, visibility into the work.
//
// Tier 3 — harness.stream_events — adds structured progress on
// choices[0].delta.harness. Standard SDKs ignore unknown keys inside delta, so
// it is safe to send to a client that has never heard of it, and a
// harness-aware UI can render every tool call as it happens.

// contentChunkRunes is how much of the final answer goes in each delta.
// Small enough that a progressive renderer looks alive, large enough not to
// produce hundreds of frames for a long reply.
const contentChunkRunes = 48

// keepaliveInterval bounds how long the connection can sit silent. Tool
// execution can legitimately take minutes, and intermediaries time out idle
// connections well before that.
const keepaliveInterval = 10 * time.Second

// sseStream writes server-sent events for one request.
//
// Every write goes through the mutex: keepalives come from a timer goroutine
// while events come from the agent's tool goroutines, and two writers
// interleaving would produce frames that no client can parse.
type sseStream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	err     error

	id      string
	model   string
	created int64
}

func newSSEStream(w http.ResponseWriter, id, model string) (*sseStream, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("the response writer cannot stream")
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which holds every event until
	// the response completes and makes streaming look broken.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &sseStream{
		w: w, flusher: flusher,
		id: id, model: model, created: time.Now().Unix(),
	}, nil
}

// send writes one chunk.
func (s *sseStream) send(chunk *oai.ChatCompletionChunk) {
	payload, err := json.Marshal(chunk)
	if err != nil {
		s.fail(err)
		return
	}
	s.raw("data: " + string(payload) + "\n\n")
}

// comment writes an SSE comment, which every client ignores. Used as a
// keepalive: it holds the connection without appearing in the message stream.
func (s *sseStream) comment(text string) {
	s.raw(": " + text + "\n\n")
}

// done writes the terminator the OpenAI SDKs look for.
func (s *sseStream) done() {
	s.raw("data: [DONE]\n\n")
}

func (s *sseStream) raw(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return
	}
	if _, err := s.w.Write([]byte(text)); err != nil {
		s.err = err
		return
	}
	s.flusher.Flush()
}

func (s *sseStream) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
}

// Err reports the first write failure, which normally means the client hung up.
func (s *sseStream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// chunk builds a chunk carrying one delta.
func (s *sseStream) chunk(delta oai.Delta, finish *string) *oai.ChatCompletionChunk {
	return &oai.ChatCompletionChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []oai.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
}

// sendRole opens the message, as OpenAI does.
func (s *sseStream) sendRole() {
	s.send(s.chunk(oai.Delta{Role: oai.RoleAssistant}, nil))
}

// sendEvent emits a harness progress event inside an otherwise empty delta.
//
// It rides on delta rather than at the top level of the chunk because SDKs
// model delta as an open map and a chunk as a fixed struct: an unknown key in
// delta is ignored, while an unknown top-level key makes stricter clients
// reject the frame.
func (s *sseStream) sendEvent(ev agent.Event) {
	payload, err := json.Marshal(ev)
	if err != nil {
		return
	}
	s.send(s.chunk(oai.Delta{Harness: payload}, nil))
}

// sendContent emits text as a series of deltas.
func (s *sseStream) sendContent(text string) {
	for _, piece := range splitRunes(text, contentChunkRunes) {
		p := piece
		s.send(s.chunk(oai.Delta{Content: &p}, nil))
	}
}

// sendFinish closes the message with a finish reason and the aggregated usage.
//
// The usage is the total across every upstream call the run made. Reporting
// only the final turn's numbers would understate a fifteen-tool-call request
// by more than an order of magnitude, and the client asked one question.
func (s *sseStream) sendFinish(reason string, usage oai.Usage, includeUsage bool) {
	s.send(s.chunk(oai.Delta{}, &reason))

	if includeUsage {
		u := usage
		final := s.chunk(oai.Delta{}, nil)
		final.Choices = []oai.ChunkChoice{}
		final.Usage = &u
		s.send(final)
	}
}

// sendError reports a failure that happened after the headers were already
// written, when it is far too late for an HTTP status code.
//
// The frame is shaped like a provider's mid-stream error, because that is what
// clients of this API already have to handle — several providers do exactly
// this, and a client that copes with them copes with us.
func (s *sseStream) sendError(apiErr *oai.APIError) {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": apiErr.Message,
			"type":    apiErr.Type,
			"code":    apiErr.Code,
			"param":   apiErr.Param,
		},
	})
	if err != nil {
		return
	}
	s.raw("data: " + string(payload) + "\n\n")
}

// keepalive holds the connection open while the agent works, until stop is
// closed. Returns a function that stops it and waits for the goroutine.
func (s *sseStream) keepalive() func() {
	stop := make(chan struct{})
	finished := make(chan struct{})

	go func() {
		defer close(finished)
		t := time.NewTicker(keepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.comment("working")
			}
		}
	}()

	return func() {
		close(stop)
		<-finished
	}
}

// splitRunes cuts text into pieces of at most n runes, never mid-rune. Cutting
// on bytes would produce invalid UTF-8 in a delta, which some clients render as
// replacement characters and others reject outright.
func splitRunes(text string, n int) []string {
	if text == "" {
		return nil
	}
	if n <= 0 || utf8.RuneCountInString(text) <= n {
		return []string{text}
	}

	var out []string
	var b strings.Builder
	count := 0
	for _, r := range text {
		b.WriteRune(r)
		count++
		if count == n {
			out = append(out, b.String())
			b.Reset()
			count = 0
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
