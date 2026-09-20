package server

import (
	"net/http"
	"strings"

	"github.com/FirePing32/harness-api/internal/oai"
)

// Four ways to say which workspace to use, because gateways disagree about
// what they forward.
//
// `/v1/chat/completions` is a stateless endpoint and an agent needs somewhere
// to work, so the binding has to travel somehow. Any single mechanism fails
// against some deployment: LiteLLM and several API gateways drop unknown
// headers but forward unknown body fields, others do exactly the reverse, and
// a few normalise the body but pass headers straight through.
//
// The model-suffix form is the one that always survives, because `model` is a
// required field that every proxy forwards and none of them interpret. It is
// ugly, and it is the fallback that makes "works with curl, not through my
// proxy" stop being a class of bug report.
//
// Precedence is fixed and documented rather than inferred: a request that
// somehow carries two of them gets the more specific one, and the order never
// depends on which arrived first.

// Binding names the workspace a request should run in.
type Binding struct {
	// SessionID reattaches to a live session.
	SessionID string
	// Workspace is a directory to bind a new session to.
	Workspace string
	// Source records which channel supplied the binding, for the access log.
	Source string
}

// Ephemeral reports that no workspace was named and a fresh one is wanted.
func (b Binding) Ephemeral() bool { return b.SessionID == "" && b.Workspace == "" }

// Harness request headers.
const (
	HeaderSession   = "X-Harness-Session"
	HeaderWorkspace = "X-Harness-Workspace"
)

// modelSessionSeparator splits a model name from a session id in the
// `model::session` form.
const modelSessionSeparator = "::"

// ResolveBinding works out which workspace a request wants.
//
// It also rewrites req.Model when the session came in through the model suffix,
// so nothing downstream has to know the suffix ever existed. Doing that here,
// at the single point the suffix is understood, is what keeps it from leaking
// into an upstream request as a model name no provider recognises.
func ResolveBinding(r *http.Request, req *oai.ChatCompletionRequest) Binding {
	if v := strings.TrimSpace(r.Header.Get(HeaderSession)); v != "" {
		return Binding{SessionID: v, Source: "header:" + HeaderSession}
	}
	if v := strings.TrimSpace(r.Header.Get(HeaderWorkspace)); v != "" {
		return Binding{Workspace: v, Source: "header:" + HeaderWorkspace}
	}

	if req.Harness != nil {
		if v := strings.TrimSpace(req.Harness.SessionID); v != "" {
			return Binding{SessionID: v, Source: "body:harness.session_id"}
		}
		if v := strings.TrimSpace(req.Harness.Workspace); v != "" {
			return Binding{Workspace: v, Source: "body:harness.workspace"}
		}
	}

	if model, session, ok := splitModelSession(req.Model); ok {
		req.Model = model
		return Binding{SessionID: session, Source: "model-suffix"}
	}

	return Binding{Source: "ephemeral"}
}

// splitModelSession separates "gpt-4.1::ws_abc" into its parts.
//
// Only the last separator is significant, so a model name that itself contains
// a colon pair still works. An empty half means the caller wrote something
// malformed, and treating that as "no binding" is better than binding to "".
func splitModelSession(model string) (string, string, bool) {
	i := strings.LastIndex(model, modelSessionSeparator)
	if i < 0 {
		return model, "", false
	}
	name := strings.TrimSpace(model[:i])
	session := strings.TrimSpace(model[i+len(modelSessionSeparator):])
	if name == "" || session == "" {
		return model, "", false
	}
	return name, session, true
}
