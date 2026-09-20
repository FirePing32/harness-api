package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/oai"
)

// newTestServer builds a Server pointed at a stub provider. The stub's handler is
// supplied by the caller so each test controls what the provider does.
func newTestServer(t *testing.T, provider http.HandlerFunc, mutate func(*config.Config)) *httptest.Server {
	t.Helper()

	upstreamSrv := httptest.NewServer(provider)
	t.Cleanup(upstreamSrv.Close)

	cfg := config.Default()
	cfg.Upstream.BaseURL = upstreamSrv.URL + "/v1"
	cfg.Upstream.Model = "test-model"
	cfg.Upstream.Timeout = config.Duration(5 * time.Second)
	cfg.Upstream.MaxRetries = 0
	// Each server gets its own workspace root, so ephemeral sessions from one
	// test cannot be seen or reclaimed by another.
	cfg.Workspace.Root = t.TempDir()
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test config invalid: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(&cfg, log)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	t.Cleanup(func() { s.Sessions().Close() })

	front := httptest.NewServer(s.Handler())
	t.Cleanup(front.Close)
	return front
}

// okProvider answers every request with a fixed successful completion.
func okProvider(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"test-model","choices":[
		{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
}

func postJSON(t *testing.T, url, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decodeError(t *testing.T, resp *http.Response) oai.ErrorEnvelope {
	t.Helper()
	var env oai.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("error body is not an OpenAI error envelope: %v", err)
	}
	return env
}

func TestChatCompletionsReturnsTheModelsAnswer(t *testing.T) {
	// No longer a passthrough: the request goes through the agent loop, which
	// happens to need one turn when the model asks for no tools.
	front := newTestServer(t, okProvider, nil)

	resp := postJSON(t, front.URL+"/v1/chat/completions",
		`{"model":"test-model","messages":[{"role":"user","content":"ping"}]}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out oai.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Choices[0].Message.Content.String() != "pong" {
		t.Errorf("content = %q", out.Choices[0].Message.Content.String())
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("every response should carry a request id for log correlation")
	}
}

func TestChatCompletionsDefaultsModel(t *testing.T) {
	var sawModel string
	front := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		sawModel, _ = req["model"].(string)
		okProvider(w, r)
	}, nil)

	resp := postJSON(t, front.URL+"/v1/chat/completions",
		`{"messages":[{"role":"user","content":"ping"}]}`, nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if sawModel != "test-model" {
		t.Errorf("upstream model = %q, want the configured default", sawModel)
	}
}

func TestChatCompletionsRejectsEmptyMessages(t *testing.T) {
	front := newTestServer(t, okProvider, nil)

	resp := postJSON(t, front.URL+"/v1/chat/completions", `{"model":"m","messages":[]}`, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if env.Error.Param != "messages" {
		t.Errorf("param = %q, want \"messages\" so clients can point at the field", env.Error.Param)
	}
	if env.Error.Type != oai.ErrTypeInvalidRequest {
		t.Errorf("type = %q, want %q", env.Error.Type, oai.ErrTypeInvalidRequest)
	}
}

func TestChatCompletionsRejectsToolMessageWithoutID(t *testing.T) {
	// A tool result that cannot be matched to its call makes strict providers
	// reject the entire request, so catching it here gives a far better message.
	front := newTestServer(t, okProvider, nil)

	resp := postJSON(t, front.URL+"/v1/chat/completions",
		`{"model":"m","messages":[{"role":"tool","content":"result"}]}`, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	env := decodeError(t, resp)
	if !strings.Contains(env.Error.Message, "tool_call_id") {
		t.Errorf("message should name the missing field, got %q", env.Error.Message)
	}
}

func TestChatCompletionsRejectsMalformedJSON(t *testing.T) {
	front := newTestServer(t, okProvider, nil)

	resp := postJSON(t, front.URL+"/v1/chat/completions", `{"model":`, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("errors must be JSON so OpenAI clients can parse them, got %q", ct)
	}
}

func TestAuthRequiredWhenTokensConfigured(t *testing.T) {
	front := newTestServer(t, okProvider, func(c *config.Config) {
		c.Server.AuthTokens = []string{"secret-token"}
	})

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	t.Run("no token", func(t *testing.T) {
		resp := postJSON(t, front.URL+"/v1/chat/completions", body, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("wrong token", func(t *testing.T) {
		resp := postJSON(t, front.URL+"/v1/chat/completions", body,
			map[string]string{"Authorization": "Bearer nope"})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
	})

	t.Run("correct token", func(t *testing.T) {
		resp := postJSON(t, front.URL+"/v1/chat/completions", body,
			map[string]string{"Authorization": "Bearer secret-token"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})

	t.Run("case-insensitive scheme", func(t *testing.T) {
		// RFC 7235 makes the scheme case-insensitive and some clients send "bearer".
		resp := postJSON(t, front.URL+"/v1/chat/completions", body,
			map[string]string{"Authorization": "bearer secret-token"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
	})
}

func TestHealthEndpointsBypassAuth(t *testing.T) {
	// A liveness probe should not need a credential, and it reveals nothing.
	front := newTestServer(t, okProvider, func(c *config.Config) {
		c.Server.AuthTokens = []string{"secret-token"}
	})

	resp, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
}

func TestUpstreamAuthFailureBecomesBadGateway(t *testing.T) {
	front := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}, nil)

	resp := postJSON(t, front.URL+"/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)

	// Relaying 401 would tell the caller their own token is wrong. It is not.
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestBodyLimitEnforced(t *testing.T) {
	front := newTestServer(t, okProvider, func(c *config.Config) {
		c.Server.MaxBodyBytes = 256
	})

	huge := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 1000) + `"}]}`
	resp := postJSON(t, front.URL+"/v1/chat/completions", huge, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestModelsEndpoint(t *testing.T) {
	front := newTestServer(t, okProvider, nil)

	resp, err := http.Get(front.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var list modelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "test-model" {
		t.Errorf("unexpected model list: %+v", list)
	}
}

func TestUnknownRequestFieldsReachProvider(t *testing.T) {
	// A passthrough server that eats provider-specific knobs is useless to anyone
	// tuning a local model through vLLM or Ollama.
	var sawBody map[string]any
	front := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sawBody)
		okProvider(w, r)
	}, nil)

	postJSON(t, front.URL+"/v1/chat/completions", `{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"chat_template_kwargs":{"enable_thinking":true}
	}`, nil)

	if _, ok := sawBody["chat_template_kwargs"]; !ok {
		t.Errorf("provider-specific field was dropped; body was %v", sawBody)
	}
}

func TestHarnessExtensionDoesNotReachProvider(t *testing.T) {
	var sawBody map[string]any
	front := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sawBody)
		okProvider(w, r)
	}, nil)

	postJSON(t, front.URL+"/v1/chat/completions", `{
		"model":"m",
		"messages":[{"role":"user","content":"hi"}],
		"harness":{"session_id":"sess_1"}
	}`, nil)

	if _, ok := sawBody["harness"]; ok {
		t.Errorf("harness extension leaked to the provider; body was %v", sawBody)
	}
}
