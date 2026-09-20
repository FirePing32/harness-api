package upstream

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prakhargurunani/harness-api/internal/config"
	"github.com/prakhargurunani/harness-api/internal/oai"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBuildBodyStripsHarnessExtension(t *testing.T) {
	maxIter := 3
	req := &oai.ChatCompletionRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
		Harness:  &oai.HarnessRequestExt{SessionID: "sess_1", MaxIterations: &maxIter},
	}

	body, err := BuildBody(req, false)
	if err != nil {
		t.Fatalf("BuildBody: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["harness"]; ok {
		t.Error("harness extension reached the provider; it is inbound-only")
	}

	// The caller's request must be untouched — the agent loop keeps using it.
	if req.Harness == nil {
		t.Error("BuildBody mutated the caller's request")
	}
}

func TestBuildBodyStripsReasoningContent(t *testing.T) {
	// DeepSeek returns 400 when it sees its own reasoning echoed back in history.
	req := &oai.ChatCompletionRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleUser, Content: oai.TextContent("q")},
			{
				Role:             oai.RoleAssistant,
				Content:          oai.TextContent("a"),
				ReasoningContent: "chain of thought",
			},
		},
	}

	body, err := BuildBody(req, false)
	if err != nil {
		t.Fatalf("BuildBody: %v", err)
	}
	if strings.Contains(string(body), "chain of thought") {
		t.Errorf("reasoning_content reached the provider:\n%s", body)
	}
	if req.Messages[1].ReasoningContent == "" {
		t.Error("BuildBody mutated the caller's history; clients still need to see reasoning")
	}
}

func TestBuildBodyStripsReasoningFromUnknownFields(t *testing.T) {
	// Some providers deliver reasoning under a different key, which the
	// unknown-field passthrough would otherwise faithfully echo back at them.
	req := &oai.ChatCompletionRequest{
		Model: "m",
		Messages: []oai.Message{{
			Role:    oai.RoleAssistant,
			Content: oai.TextContent("a"),
			Extra: map[string]json.RawMessage{
				"thinking":     json.RawMessage(`"secret reasoning"`),
				"harmless_key": json.RawMessage(`"keep me"`),
			},
		}},
	}

	body, err := BuildBody(req, false)
	if err != nil {
		t.Fatalf("BuildBody: %v", err)
	}
	if strings.Contains(string(body), "secret reasoning") {
		t.Errorf("banned key survived in Extra:\n%s", body)
	}
	if !strings.Contains(string(body), "keep me") {
		t.Errorf("unrelated provider fields must still pass through:\n%s", body)
	}
}

func TestBuildBodySetsStreamFlag(t *testing.T) {
	req := &oai.ChatCompletionRequest{
		Model:         "m",
		Messages:      []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
		StreamOptions: &oai.StreamOptions{IncludeUsage: true},
	}

	nonStream, _ := BuildBody(req, false)
	if strings.Contains(string(nonStream), `"stream":true`) {
		t.Error("non-streaming body should not set stream")
	}
	if strings.Contains(string(nonStream), "stream_options") {
		t.Error("stream_options is meaningless without streaming and trips strict providers")
	}

	streaming, _ := BuildBody(req, true)
	if !strings.Contains(string(streaming), `"stream":true`) {
		t.Errorf("streaming body missing stream flag:\n%s", streaming)
	}
}

func TestCompleteHappyPath(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[
			{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
	}))
	defer srv.Close()

	c := New(config.Upstream{
		BaseURL: srv.URL + "/v1",
		APIKey:  "sk-test",
		Timeout: config.Duration(5 * time.Second),
	}, discardLogger())

	resp, err := c.Complete(context.Background(), &oai.ChatCompletionRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("ping")}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content.String() != "pong" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 6 {
		t.Errorf("usage not parsed: %+v", resp.Usage)
	}
}

func TestCompleteRetriesOn503ThenSucceeds(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":{"message":"overloaded"}}`)
			return
		}
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	c := New(config.Upstream{
		BaseURL:    srv.URL + "/v1",
		Timeout:    config.Duration(5 * time.Second),
		MaxRetries: 3,
	}, discardLogger())

	resp, err := c.Complete(context.Background(), &oai.ChatCompletionRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if resp.Choices[0].Message.Content.String() != "ok" {
		t.Errorf("unexpected content")
	}
}

func TestCompleteDoesNotRetryOn400(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unknown model"}}`)
	}))
	defer srv.Close()

	c := New(config.Upstream{
		BaseURL:    srv.URL + "/v1",
		Timeout:    config.Duration(5 * time.Second),
		MaxRetries: 3,
	}, discardLogger())

	_, err := c.Complete(context.Background(), &oai.ChatCompletionRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	// A 400 will be a 400 again; retrying just delays the failure.
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 — a 400 is not retryable", attempts)
	}
}

func TestUpstreamErrorBodyIsRedacted(t *testing.T) {
	// Several providers echo the request they received, Authorization header and
	// all, inside their error bodies.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"bad request, received Authorization: Bearer sk-live-abcdef123456"}}`)
	}))
	defer srv.Close()

	c := New(config.Upstream{
		BaseURL: srv.URL + "/v1",
		Timeout: config.Duration(5 * time.Second),
	}, discardLogger())

	_, err := c.Complete(context.Background(), &oai.ChatCompletionRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sk-live-abcdef123456") {
		t.Errorf("credential leaked through the error path: %v", err)
	}
}

func TestToAPIErrorMapsUpstreamAuthToBadGateway(t *testing.T) {
	// A 401 from the provider means THIS SERVER's key is wrong. Relaying 401 would
	// tell the caller to fix their own token, which is the opposite of the truth.
	apiErr := ToAPIError(&Error{Status: http.StatusUnauthorized, Body: "invalid api key"})
	if apiErr.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", apiErr.Status)
	}
	if !strings.Contains(apiErr.Message, "this server's credentials") {
		t.Errorf("message should point the operator at the server config, got %q", apiErr.Message)
	}
}

func TestToAPIErrorMapsStatuses(t *testing.T) {
	tests := []struct {
		upstreamStatus int
		wantStatus     int
	}{
		{http.StatusBadRequest, http.StatusBadRequest},
		{http.StatusNotFound, http.StatusBadRequest},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
		{http.StatusInternalServerError, http.StatusBadGateway},
		{http.StatusBadGateway, http.StatusBadGateway},
	}
	for _, tt := range tests {
		got := ToAPIError(&Error{Status: tt.upstreamStatus, Body: "x"})
		if got.Status != tt.wantStatus {
			t.Errorf("upstream %d mapped to %d, want %d", tt.upstreamStatus, got.Status, tt.wantStatus)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("5"); got != 5*time.Second {
		t.Errorf("seconds form: got %v, want 5s", got)
	}
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("empty: got %v, want 0", got)
	}
	if got := parseRetryAfter("garbage"); got != 0 {
		t.Errorf("unparseable: got %v, want 0", got)
	}
	// An absolute date already in the past must not produce a negative wait.
	if got := parseRetryAfter("Mon, 02 Jan 2006 15:04:05 GMT"); got != 0 {
		t.Errorf("past date: got %v, want 0", got)
	}
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future); got <= 0 || got > 31*time.Second {
		t.Errorf("future date: got %v, want ~30s", got)
	}
}

func TestBackoffRespectsRetryAfter(t *testing.T) {
	got := backoff(1, &Error{Status: 429, RetryAfter: 7 * time.Second})
	if got != 7*time.Second {
		t.Errorf("backoff = %v, want the provider's Retry-After of 7s", got)
	}
	// And it must be capped, so a hostile or buggy header cannot park a request
	// for an hour.
	got = backoff(1, &Error{Status: 429, RetryAfter: time.Hour})
	if got > time.Minute {
		t.Errorf("backoff = %v, want it capped at 60s", got)
	}
}

func TestCompleteHonoursContextCancellation(t *testing.T) {
	// The handler must be released by the test, not by r.Context(): Go's HTTP/1.1
	// server does not observe a client disconnect until it next reads or writes the
	// socket, so waiting on the request context here would block Server.Close
	// indefinitely rather than exercising anything.
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Defers run last-in-first-out, so the handler is freed before Close waits on it.
	defer srv.Close()
	defer close(release)

	c := New(config.Upstream{
		BaseURL: srv.URL + "/v1",
		Timeout: config.Duration(5 * time.Second),
	}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := c.Complete(ctx, &oai.ChatCompletionRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestCompleteReportsNonJSONSuccessBody(t *testing.T) {
	// A misconfigured proxy answering 200 with an HTML login page is a real and
	// confusing failure; the error should name the shape of the problem.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "<html><body>Please log in</body></html>")
	}))
	defer srv.Close()

	c := New(config.Upstream{
		BaseURL: srv.URL + "/v1",
		Timeout: config.Duration(5 * time.Second),
	}, discardLogger())

	_, err := c.Complete(context.Background(), &oai.ChatCompletionRequest{
		Model:    "m",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "non-JSON") {
		t.Errorf("error should identify the failure mode, got: %v", err)
	}
}
