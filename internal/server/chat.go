package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/upstream"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// handleChatCompletions serves POST /v1/chat/completions.
//
// The request is run through the agent loop rather than forwarded, so a single
// call may produce many upstream calls. The reported usage is the total across
// all of them.
//
// Session binding is still minimal: every request gets a fresh ephemeral
// workspace, which is destroyed when it goes idle. Reattaching to an existing
// session over several requests arrives with the binding resolver.
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
		// Streaming an agent run is not the same problem as streaming a
		// completion — intermediate turns have to be buffered or the answer
		// arrives interleaved with the model's tool-calling preamble. Saying so
		// plainly beats a hang or a silently non-streamed response.
		oai.WriteError(w, oai.NewInvalidRequest(
			"streaming is not implemented yet; retry with \"stream\": false", "stream"), reqID)
		return
	}

	session, release, err := s.acquireSession(req)
	if err != nil {
		oai.WriteError(w, err, reqID)
		return
	}
	defer release()

	result, err := s.agent.Run(r.Context(), session, req)
	if err != nil {
		if r.Context().Err() != nil {
			// The caller hung up. Nothing to write to, and nothing worth logging
			// as an error.
			s.log.Debug("client disconnected during the agent run", "request_id", reqID)
			return
		}
		var apiErr *oai.APIError
		if errors.As(err, &apiErr) {
			oai.WriteError(w, apiErr, reqID)
			return
		}
		s.log.Error("agent run failed",
			"request_id", reqID, "session_id", session.ID(), "error", err)
		oai.WriteError(w, upstream.ToAPIError(err), reqID)
		return
	}

	s.log.Info("agent run complete",
		"request_id", reqID,
		"session_id", session.ID(),
		"stop", result.Stop,
		"turns", result.Turns,
		"total_tokens", result.Usage.TotalTokens,
		"duration_ms", result.Elapsed.Milliseconds())

	// The session id goes back in a header so a caller can reattach to this
	// workspace, even though nothing reads it back yet.
	w.Header().Set("X-Harness-Session", session.ID())

	writeJSON(w, http.StatusOK, result.ToResponse(reqID, req.Model, time.Now().Unix()))
}

// acquireSession resolves the workspace for a request and locks it.
//
// The returned release function must always be called. The session is held for
// the whole run rather than per tool call: tools mutate both the filesystem and
// the observation ledger, and two requests interleaving on one workspace would
// produce failures nobody could reconstruct from a transcript.
func (s *Server) acquireSession(req *oai.ChatCompletionRequest) (*workspace.Session, func(), error) {
	if req.Harness != nil && req.Harness.SessionID != "" {
		session, ok := s.sessions.Get(req.Harness.SessionID)
		if !ok {
			return nil, nil, oai.NewInvalidRequest(
				"unknown or expired session "+req.Harness.SessionID+
					"; omit harness.session_id to start a new one", "harness.session_id")
		}
		if !session.TryLock() {
			// Blocking would mean waiting out another request's agent loop, which
			// can legitimately run for minutes.
			return nil, nil, &oai.APIError{
				Status: http.StatusConflict, Type: "invalid_request_error",
				Message: "this session is already handling another request",
				Param:   "harness.session_id",
			}
		}
		session.Touch()
		return session, session.Unlock, nil
	}

	var (
		session *workspace.Session
		err     error
	)
	if req.Harness != nil && req.Harness.Workspace != "" {
		session, err = s.sessions.CreateAt(req.Harness.Workspace)
		if err != nil {
			return nil, nil, oai.NewInvalidRequest(
				"could not use that workspace: "+err.Error(), "harness.workspace")
		}
	} else {
		session, err = s.sessions.CreateEphemeral()
		if err != nil {
			return nil, nil, err
		}
	}

	session.Lock()
	return session, session.Unlock, nil
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
