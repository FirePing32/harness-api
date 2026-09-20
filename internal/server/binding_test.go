package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/FirePing32/harness-api/internal/oai"
)

func bindingFor(t *testing.T, headers map[string]string, req *oai.ChatCompletionRequest) Binding {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return ResolveBinding(r, req)
}

func TestBindingPrecedence(t *testing.T) {
	// Fixed and documented rather than inferred. A request carrying several
	// channels gets the most specific one, and the answer never depends on
	// which arrived first.
	req := &oai.ChatCompletionRequest{
		Model: "gpt-4.1::ws_from_model",
		Harness: &oai.HarnessRequestExt{
			SessionID: "ws_from_body",
			Workspace: "/from/body",
		},
	}
	got := bindingFor(t, map[string]string{
		HeaderSession:   "ws_from_header",
		HeaderWorkspace: "/from/header",
	}, req)

	if got.SessionID != "ws_from_header" {
		t.Errorf("SessionID = %q, want the session header to win", got.SessionID)
	}
}

func TestBindingFallsThroughEachChannel(t *testing.T) {
	cases := []struct {
		name     string
		headers  map[string]string
		req      *oai.ChatCompletionRequest
		wantSess string
		wantWork string
		wantSrc  string
	}{
		{
			name:     "session header",
			headers:  map[string]string{HeaderSession: "ws_a"},
			req:      &oai.ChatCompletionRequest{Model: "m"},
			wantSess: "ws_a", wantSrc: "header:" + HeaderSession,
		},
		{
			name:     "workspace header",
			headers:  map[string]string{HeaderWorkspace: "/proj"},
			req:      &oai.ChatCompletionRequest{Model: "m"},
			wantWork: "/proj", wantSrc: "header:" + HeaderWorkspace,
		},
		{
			name: "body session id",
			req: &oai.ChatCompletionRequest{Model: "m",
				Harness: &oai.HarnessRequestExt{SessionID: "ws_b"}},
			wantSess: "ws_b", wantSrc: "body:harness.session_id",
		},
		{
			name: "body workspace",
			req: &oai.ChatCompletionRequest{Model: "m",
				Harness: &oai.HarnessRequestExt{Workspace: "/proj"}},
			wantWork: "/proj", wantSrc: "body:harness.workspace",
		},
		{
			name:     "model suffix",
			req:      &oai.ChatCompletionRequest{Model: "gpt-4.1::ws_c"},
			wantSess: "ws_c", wantSrc: "model-suffix",
		},
		{
			name:    "nothing at all",
			req:     &oai.ChatCompletionRequest{Model: "m"},
			wantSrc: "ephemeral",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := bindingFor(t, c.headers, c.req)
			if got.SessionID != c.wantSess {
				t.Errorf("SessionID = %q, want %q", got.SessionID, c.wantSess)
			}
			if got.Workspace != c.wantWork {
				t.Errorf("Workspace = %q, want %q", got.Workspace, c.wantWork)
			}
			if got.Source != c.wantSrc {
				t.Errorf("Source = %q, want %q", got.Source, c.wantSrc)
			}
		})
	}
}

func TestBindingStripsTheSuffixFromTheModel(t *testing.T) {
	// The whole point of doing this at the single place the suffix is
	// understood: nothing downstream should ever see a model name no provider
	// recognises.
	req := &oai.ChatCompletionRequest{Model: "gpt-4.1::ws_abc"}

	got := bindingFor(t, nil, req)

	if req.Model != "gpt-4.1" {
		t.Errorf("Model = %q, want the suffix removed", req.Model)
	}
	if got.SessionID != "ws_abc" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
}

func TestBindingLeavesOrdinaryModelNamesAlone(t *testing.T) {
	for _, model := range []string{
		"gpt-4.1",
		"accounts/fireworks/models/llama-v3",
		"ft:gpt-4o:org:name:id", // single colons must not be misread
		"gpt-4.1::",             // malformed: empty session
		"::ws_abc",              // malformed: empty model
	} {
		req := &oai.ChatCompletionRequest{Model: model}
		got := bindingFor(t, nil, req)

		if req.Model != model {
			t.Errorf("Model %q was rewritten to %q", model, req.Model)
		}
		if got.SessionID != "" {
			t.Errorf("model %q produced a session id %q", model, got.SessionID)
		}
	}
}

func TestBindingUsesTheLastSeparator(t *testing.T) {
	// A model name containing a separator of its own still works.
	req := &oai.ChatCompletionRequest{Model: "org::model::ws_xyz"}

	got := bindingFor(t, nil, req)

	if req.Model != "org::model" {
		t.Errorf("Model = %q, want org::model", req.Model)
	}
	if got.SessionID != "ws_xyz" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
}

func TestBindingTrimsWhitespace(t *testing.T) {
	got := bindingFor(t, map[string]string{HeaderSession: "  ws_a  "},
		&oai.ChatCompletionRequest{Model: "m"})
	if got.SessionID != "ws_a" {
		t.Errorf("SessionID = %q, want it trimmed", got.SessionID)
	}

	// A header present but empty must not bind to "".
	got = bindingFor(t, map[string]string{HeaderSession: "   "},
		&oai.ChatCompletionRequest{Model: "m"})
	if !got.Ephemeral() {
		t.Errorf("an empty header produced a binding: %+v", got)
	}
}

func TestSplitRunesNeverCutsMidRune(t *testing.T) {
	// Cutting on bytes would produce invalid UTF-8 inside a delta, which some
	// clients render as replacement characters and others reject outright.
	text := "héllo wörld — ünïcödé ✓ 日本語テキスト"

	pieces := splitRunes(text, 5)

	var rebuilt string
	for _, p := range pieces {
		rebuilt += p
		if len([]rune(p)) > 5 {
			t.Errorf("piece %q is longer than the limit", p)
		}
	}
	if rebuilt != text {
		t.Errorf("rejoined = %q, want the original", rebuilt)
	}
}

func TestSplitRunesEdgeCases(t *testing.T) {
	if got := splitRunes("", 10); got != nil {
		t.Errorf("empty text = %v, want nil", got)
	}
	if got := splitRunes("short", 10); len(got) != 1 || got[0] != "short" {
		t.Errorf("text under the limit = %v, want one piece", got)
	}
	if got := splitRunes("abc", 0); len(got) != 1 {
		t.Errorf("a zero limit should not split: %v", got)
	}
}
