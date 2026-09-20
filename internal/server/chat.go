package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/upstream"
)

// handleChatCompletions serves POST /v1/chat/completions.
//
// At this phase the handler is a faithful proxy: decode, validate, forward,
// relay. The agent loop slots in where forwardOnce is called, once the tools and
// workspace exist. Proving the schema against real OpenAI clients before adding
// agent complexity means any later failure is unambiguously the agent's fault.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqID := RequestIDFromContext(r.Context())

	req, err := decodeChatRequest(r)
	if err != nil {
		oai.WriteError(w, err, reqID)
		return
	}

	if req.Model == "" {
		req.Model = s.upstream.DefaultModel()
	}
	if req.Model == "" {
		oai.WriteError(w, oai.NewInvalidRequest(
			"no model specified and the server has no default model configured "+
				"(set upstream.model or --upstream-model)", "model"), reqID)
		return
	}

	if req.Stream {
		// Streaming arrives in the next phase. Saying so plainly beats a hang or a
		// silently non-streamed response that clients will misparse.
		oai.WriteError(w, oai.NewInvalidRequest(
			"streaming is not implemented yet; retry with \"stream\": false", "stream"), reqID)
		return
	}

	resp, err := s.upstream.Complete(r.Context(), req)
	if err != nil {
		if errors.Is(err, r.Context().Err()) && r.Context().Err() != nil {
			// The caller hung up. Nothing to write to, and nothing worth logging
			// as an error.
			s.log.Debug("client disconnected before upstream responded", "request_id", reqID)
			return
		}
		s.log.Error("upstream completion failed", "request_id", reqID, "error", err)
		oai.WriteError(w, upstream.ToAPIError(err), reqID)
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// decodeChatRequest parses and validates a chat completion request body.
func decodeChatRequest(r *http.Request) (*oai.ChatCompletionRequest, error) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !isJSONContentType(ct) {
		return nil, oai.NewInvalidRequest(
			"Content-Type must be application/json, got "+ct, "")
	}

	var req oai.ChatCompletionRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return nil, oai.NewInvalidRequest("request body is too large", "")
		}
		return nil, oai.NewInvalidRequest("could not parse request body as JSON: "+err.Error(), "")
	}

	if len(req.Messages) == 0 {
		return nil, oai.NewInvalidRequest("messages must contain at least one message", "messages")
	}
	for i, m := range req.Messages {
		if m.Role == "" {
			return nil, oai.NewInvalidRequest(
				"every message needs a role", "messages["+itoa(i)+"].role")
		}
		if m.Role == oai.RoleTool && m.ToolCallID == "" {
			// A tool message with no id cannot be matched to its call, and strict
			// providers reject the whole request rather than just that message.
			return nil, oai.NewInvalidRequest(
				"tool messages must carry the tool_call_id they answer",
				"messages["+itoa(i)+"].tool_call_id")
		}
	}

	if req.N != nil && *req.N > 1 {
		// An agent loop runs one conversation. Returning n>1 would mean n divergent
		// tool-call histories, which has no coherent meaning here.
		return nil, oai.NewInvalidRequest("n must be 1; this server runs a single agent loop per request", "n")
	}

	return &req, nil
}

func isJSONContentType(ct string) bool {
	// Tolerate parameters such as "; charset=utf-8".
	for i := 0; i < len(ct); i++ {
		if ct[i] == ';' {
			ct = ct[:i]
			break
		}
	}
	switch trimSpace(ct) {
	case "application/json", "application/vnd.api+json", "text/json":
		return true
	}
	return false
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
