package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/FirePing32/harness-api/internal/agent"
	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/upstream"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// handleChatCompletions serves POST /v1/chat/completions.
//
// The request is run through the agent loop rather than forwarded, so a single
// call may produce many upstream calls. The reported usage is the total across
// all of them.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqID := RequestIDFromContext(r.Context())

	req, err := decodeChatRequest(r)
	if err != nil {
		oai.WriteError(w, err, reqID)
		return
	}

	// Resolved before the model default is applied, because the model string is
	// one of the channels a session id can arrive through and the suffix has to
	// come off before anything treats the remainder as a model name.
	binding := ResolveBinding(r, req)

	if req.Model == "" {
		req.Model = s.upstream.DefaultModel()
	}
	if req.Model == "" {
		oai.WriteError(w, oai.NewInvalidRequest(
			"no model specified and the server has no default model configured "+
				"(set upstream.model or --upstream-model)", "model"), reqID)
		return
	}

	session, release, err := s.acquireSession(binding)
	if err != nil {
		oai.WriteError(w, err, reqID)
		return
	}
	defer release()

	// Set before anything is written, so it survives into a streaming response
	// where headers are flushed with the very first frame.
	w.Header().Set(HeaderSession, session.ID())

	log := s.log.With(
		"request_id", reqID,
		"session_id", session.ID(),
		"binding", binding.Source)

	if req.Stream {
		s.streamAgentRun(w, r, req, session, log)
		return
	}

	result, err := s.agent.Run(r.Context(), session, req)
	if err != nil {
		if r.Context().Err() != nil {
			// The caller hung up. Nothing to write to, and nothing worth logging
			// as an error.
			log.Debug("client disconnected during the agent run")
			return
		}
		var apiErr *oai.APIError
		if errors.As(err, &apiErr) {
			oai.WriteError(w, apiErr, reqID)
			return
		}
		log.Error("agent run failed", "error", err)
		oai.WriteError(w, upstream.ToAPIError(err), reqID)
		return
	}

	log.Info("agent run complete",
		"stop", result.Stop,
		"turns", result.Turns,
		"total_tokens", result.Usage.TotalTokens,
		"duration_ms", result.Elapsed.Milliseconds())

	writeJSON(w, http.StatusOK, result.ToResponse(reqID, req.Model, time.Now().Unix()))
}

// streamAgentRun serves a streaming request.
//
// The headers go out immediately so the client sees a 200 and stops waiting,
// then the connection is held with keepalive comments while the agent works.
// Once the headers are written there is no way back to an HTTP status code, so
// every failure after this point has to be reported inside the stream.
func (s *Server) streamAgentRun(
	w http.ResponseWriter, r *http.Request,
	req *oai.ChatCompletionRequest, session *workspace.Session, log *slog.Logger,
) {
	reqID := RequestIDFromContext(r.Context())

	stream, err := newSSEStream(w, reqID, req.Model)
	if err != nil {
		oai.WriteError(w, err, reqID)
		return
	}
	stream.sendRole()

	var emit agent.Emit
	if req.Harness != nil && req.Harness.StreamEvents {
		emit = stream.sendEvent
	}

	stopKeepalive := stream.keepalive()
	result, runErr := s.agent.RunWithEvents(r.Context(), session, req, emit)
	stopKeepalive()

	if runErr != nil {
		if r.Context().Err() != nil {
			log.Debug("client disconnected during the agent run")
			return
		}
		var apiErr *oai.APIError
		if !errors.As(runErr, &apiErr) {
			log.Error("agent run failed", "error", runErr)
			apiErr = upstream.ToAPIError(runErr)
		}
		stream.sendError(apiErr)
		stream.done()
		return
	}

	stream.sendContent(result.Final.Content.String())
	stream.sendFinish(result.Stop.FinishReason(), result.Usage, includeUsage(req))
	stream.done()

	if err := stream.Err(); err != nil {
		log.Debug("client disconnected while the answer was being written", "error", err)
		return
	}

	log.Info("agent run complete",
		"stop", result.Stop,
		"turns", result.Turns,
		"total_tokens", result.Usage.TotalTokens,
		"duration_ms", result.Elapsed.Milliseconds(),
		"streamed", true)
}

// includeUsage reports whether the caller asked for a usage frame. OpenAI made
// this opt-in because the extra final chunk breaks clients that assume the
// stream ends at the finish reason.
func includeUsage(req *oai.ChatCompletionRequest) bool {
	return req.StreamOptions != nil && req.StreamOptions.IncludeUsage
}

// acquireSession resolves the workspace for a request and locks it.
//
// The returned release function must always be called. The session is held for
// the whole run rather than per tool call: tools mutate both the filesystem and
// the observation ledger, and two requests interleaving on one workspace would
// produce failures nobody could reconstruct from a transcript.
func (s *Server) acquireSession(b Binding) (*workspace.Session, func(), error) {
	if b.SessionID != "" {
		session, ok := s.sessions.Get(b.SessionID)
		if !ok {
			return nil, nil, oai.NewInvalidRequest(
				"unknown or expired session "+b.SessionID+
					"; omit the session to start a new one", b.Source)
		}
		if !session.TryLock() {
			// Blocking would mean waiting out another request's agent loop, which
			// can legitimately run for minutes.
			return nil, nil, &oai.APIError{
				Status: http.StatusConflict, Type: "invalid_request_error",
				Message: "this session is already handling another request",
				Param:   b.Source,
			}
		}
		session.Touch()
		return session, session.Unlock, nil
	}

	var (
		session *workspace.Session
		err     error
	)
	if b.Workspace != "" {
		session, err = s.sessions.CreateAt(b.Workspace)
		if err != nil {
			return nil, nil, oai.NewInvalidRequest(
				"could not use that workspace: "+err.Error(), b.Source)
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
