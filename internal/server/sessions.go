package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// Managing workspaces explicitly.
//
// A session is created implicitly by any chat request, so these endpoints are
// not required to use the server. They exist for the two things that are
// awkward without them: starting a workspace before there is anything to say
// to the model, and releasing one the moment the work is done rather than
// waiting out the idle timeout. The second matters because an ephemeral
// workspace holds disk until it is reclaimed.

// SessionResponse is the public view of a session.
type SessionResponse struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Workspace string `json:"workspace"`
	// Ephemeral reports whether this server created the directory and will
	// therefore delete it. A session bound to a caller's own directory is
	// never ephemeral and that directory is never removed.
	Ephemeral bool  `json:"ephemeral"`
	CreatedAt int64 `json:"created_at"`
	LastUsed  int64 `json:"last_used"`
	// ExpiresAt is when the session will be reclaimed if it stays idle. It
	// moves forward on every request that touches the session.
	ExpiresAt int64 `json:"expires_at"`
}

func (s *Server) sessionResponse(sess *workspace.Session) SessionResponse {
	return SessionResponse{
		ID:        sess.ID(),
		Object:    "harness.session",
		Workspace: sess.Root(),
		Ephemeral: sess.Ephemeral(),
		CreatedAt: sess.CreatedAt().Unix(),
		LastUsed:  sess.LastUsed().Unix(),
		ExpiresAt: sess.LastUsed().Add(s.sessions.IdleTTL()).Unix(),
	}
}

type createSessionRequest struct {
	// Workspace binds the session to an existing directory. Empty creates a
	// fresh one owned by this server.
	Workspace string `json:"workspace,omitempty"`
}

// handleCreateSession serves POST /v1/sessions.
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	reqID := RequestIDFromContext(r.Context())

	var body createSessionRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			oai.WriteError(w, oai.NewInvalidRequest(
				"could not parse request body: "+err.Error(), ""), reqID)
			return
		}
	}

	var (
		session *workspace.Session
		err     error
	)
	if body.Workspace != "" {
		session, err = s.sessions.CreateAt(body.Workspace)
		if err != nil {
			oai.WriteError(w, oai.NewInvalidRequest(
				"could not use that workspace: "+err.Error(), "workspace"), reqID)
			return
		}
	} else {
		session, err = s.sessions.CreateEphemeral()
		if err != nil {
			s.log.Error("creating a session", "request_id", reqID, "error", err)
			oai.WriteError(w, err, reqID)
			return
		}
	}

	w.Header().Set(HeaderSession, session.ID())
	writeJSON(w, http.StatusCreated, s.sessionResponse(session))
}

// handleListSessions serves GET /v1/sessions.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	live := s.sessions.List()
	out := make([]SessionResponse, 0, len(live))
	for _, sess := range live {
		out = append(out, s.sessionResponse(sess))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   out,
	})
}

// handleGetSession serves GET /v1/sessions/{id}.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	reqID := RequestIDFromContext(r.Context())

	id := r.PathValue("id")
	session, ok := s.sessions.Get(id)
	if !ok {
		oai.WriteError(w, oai.NewNotFound("no session "+id), reqID)
		return
	}
	writeJSON(w, http.StatusOK, s.sessionResponse(session))
}

// handleDeleteSession serves DELETE /v1/sessions/{id}.
//
// Deleting a session releases its workspace, and for an ephemeral one that
// means removing the directory. A session bound to a caller's own directory
// keeps the directory; only the handle goes away.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	reqID := RequestIDFromContext(r.Context())

	id := r.PathValue("id")
	session, ok := s.sessions.Get(id)
	if !ok {
		oai.WriteError(w, oai.NewNotFound("no session "+id), reqID)
		return
	}

	// Refuse rather than pull the workspace out from under a request that is
	// still using it. The lock is held for a whole agent run, which can be
	// minutes long.
	if !session.TryLock() {
		oai.WriteError(w, &oai.APIError{
			Status: http.StatusConflict, Type: "invalid_request_error",
			Message: "this session is handling a request; try again when it finishes",
		}, reqID)
		return
	}
	session.Unlock()

	ephemeral := session.Ephemeral()
	if err := s.sessions.Remove(id); err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			oai.WriteError(w, oai.NewNotFound("no session "+id), reqID)
			return
		}
		s.log.Error("removing a session", "request_id", reqID, "session_id", id, "error", err)
		oai.WriteError(w, err, reqID)
		return
	}

	s.log.Info("session deleted", "request_id", reqID, "session_id", id)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "harness.session.deleted",
		"deleted": true,
		// Said explicitly, because "deleted" is ambiguous about whether the
		// caller's files went with it.
		"workspace_removed": ephemeral,
	})
}
