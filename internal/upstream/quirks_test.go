package upstream

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/oai"
)

func profile(name string) config.Profile {
	p, ok := config.LookupProfile(name)
	if !ok {
		panic("unknown test profile " + name)
	}
	return p
}

// bodyOf applies a profile and serialises, returning the decoded wire object —
// the bytes a provider would actually receive.
func bodyOf(t *testing.T, p config.Profile, req *oai.ChatCompletionRequest) map[string]any {
	t.Helper()
	raw, err := BuildBody(Apply(p, req), false)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func userReq(model string) *oai.ChatCompletionRequest {
	return &oai.ChatCompletionRequest{
		Model:    model,
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
	}
}

func TestApplyMovesTheTokenLimitToTheRightField(t *testing.T) {
	// A caller may set either field; it has to end up in the one this provider
	// reads, and only in that one.
	limit := 256

	req := userReq("m")
	req.MaxTokens = &limit
	body := bodyOf(t, profile("openai"), req)
	if body["max_completion_tokens"] != float64(256) {
		t.Errorf("max_completion_tokens = %v, want 256", body["max_completion_tokens"])
	}
	if _, present := body["max_tokens"]; present {
		t.Error("max_tokens was sent alongside max_completion_tokens")
	}

	req = userReq("m")
	req.MaxCompletionTokens = &limit
	body = bodyOf(t, profile("deepseek"), req)
	if body["max_tokens"] != float64(256) {
		t.Errorf("max_tokens = %v, want 256", body["max_tokens"])
	}
	if _, present := body["max_completion_tokens"]; present {
		t.Error("max_completion_tokens was sent to a provider that wants max_tokens")
	}
}

func TestApplyRenamesTheSystemRole(t *testing.T) {
	req := &oai.ChatCompletionRequest{
		Model: "o3",
		Messages: []oai.Message{
			{Role: oai.RoleSystem, Content: oai.TextContent("be terse")},
			{Role: oai.RoleUser, Content: oai.TextContent("hi")},
		},
	}

	body := bodyOf(t, profile("openai-reasoning"), req)
	msgs := body["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "developer" {
		t.Errorf("role = %v, want developer", first["role"])
	}
	if first["content"] != "be terse" {
		t.Errorf("the system text was altered: %v", first["content"])
	}
}

func TestApplyFoldsSystemIntoUserWhenThereIsNoSystemRole(t *testing.T) {
	// Dropping it would remove the tool instructions and leave the model
	// guessing at how to work, which looks like the model being bad.
	p := profile("generic")
	p.SystemRole = oai.RoleUser

	req := &oai.ChatCompletionRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleSystem, Content: oai.TextContent("you are a coding agent")},
			{Role: oai.RoleUser, Content: oai.TextContent("fix the bug")},
		},
	}

	body := bodyOf(t, p, req)
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want the system message folded in", len(msgs))
	}
	only := msgs[0].(map[string]any)
	if only["role"] != "user" {
		t.Errorf("role = %v", only["role"])
	}
	text, _ := only["content"].(string)
	if !strings.Contains(text, "coding agent") || !strings.Contains(text, "fix the bug") {
		t.Errorf("content = %q, want both texts preserved", text)
	}
}

func TestApplyFoldsSystemEvenWithNoUserMessage(t *testing.T) {
	p := profile("generic")
	p.SystemRole = oai.RoleUser

	req := &oai.ChatCompletionRequest{
		Model: "m",
		Messages: []oai.Message{
			{Role: oai.RoleSystem, Content: oai.TextContent("instructions")},
			{Role: oai.RoleAssistant, Content: oai.TextContent("ok")},
		},
	}

	body := bodyOf(t, p, req)
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if msgs[0].(map[string]any)["role"] != "user" {
		t.Error("a user message should have been inserted to carry the instructions")
	}
}

func TestApplyToolCallContentStyles(t *testing.T) {
	// Providers disagree about what an assistant message carrying tool calls
	// should have for content, and sending the wrong one fails on the *next*
	// request rather than this one.
	assistant := oai.Message{
		Role:      oai.RoleAssistant,
		Content:   oai.NullContent(),
		ToolCalls: []oai.ToolCall{{ID: "c1", Type: "function"}},
	}

	cases := []struct {
		style   config.ContentStyle
		present bool
		want    any
	}{
		{config.StyleNull, true, nil},
		{config.StyleEmpty, true, ""},
		{config.StyleOmit, false, nil},
	}

	for _, c := range cases {
		p := profile("generic")
		p.ToolCallContent = c.style

		req := &oai.ChatCompletionRequest{Model: "m", Messages: []oai.Message{assistant}}
		body := bodyOf(t, p, req)
		msg := body["messages"].([]any)[0].(map[string]any)

		got, present := msg["content"]
		if present != c.present {
			t.Errorf("style %q: content present = %v, want %v", c.style, present, c.present)
			continue
		}
		if present && got != c.want {
			t.Errorf("style %q: content = %v, want %v", c.style, got, c.want)
		}
	}
}

func TestApplyKeepsRealContentAlongsideToolCalls(t *testing.T) {
	// The style only governs an *empty* content. Text the model actually
	// produced must survive whatever the provider prefers.
	p := profile("generic")
	p.ToolCallContent = config.StyleOmit

	req := &oai.ChatCompletionRequest{Model: "m", Messages: []oai.Message{{
		Role:      oai.RoleAssistant,
		Content:   oai.TextContent("Let me check that."),
		ToolCalls: []oai.ToolCall{{ID: "c1", Type: "function"}},
	}}}

	body := bodyOf(t, p, req)
	msg := body["messages"].([]any)[0].(map[string]any)
	if msg["content"] != "Let me check that." {
		t.Errorf("content = %v, want the real text kept", msg["content"])
	}
}

func TestApplyDropsSamplingForReasoningModels(t *testing.T) {
	// Reasoning models reject these rather than ignoring them, so a request
	// that merely passed temperature through fails entirely.
	temp, topP := 0.7, 0.9
	req := userReq("o3")
	req.Temperature = &temp
	req.TopP = &topP

	body := bodyOf(t, profile("openai-reasoning"), req)
	for _, key := range []string{"temperature", "top_p"} {
		if _, present := body[key]; present {
			t.Errorf("%s was sent to a reasoning model", key)
		}
	}

	// ...and is kept for a model that accepts it.
	body = bodyOf(t, profile("openai"), userReqWith(temp))
	if body["temperature"] != 0.7 {
		t.Errorf("temperature = %v, want it preserved", body["temperature"])
	}
}

func userReqWith(temp float64) *oai.ChatCompletionRequest {
	req := userReq("gpt-4.1")
	req.Temperature = &temp
	return req
}

func TestApplyDropsUnsupportedFields(t *testing.T) {
	yes := true
	req := userReq("m")
	req.ParallelToolCalls = &yes
	req.Tools = []oai.Tool{{
		Type: "function",
		Function: oai.FunctionDef{
			Name: "read", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &yes,
		},
	}}
	req.StreamOptions = &oai.StreamOptions{IncludeUsage: true}

	// generic supports none of these.
	body := bodyOf(t, profile("generic"), req)
	if _, present := body["parallel_tool_calls"]; present {
		t.Error("parallel_tool_calls was sent to a provider that does not accept it")
	}
	fn := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if _, present := fn["strict"]; present {
		t.Error("strict was sent to a provider that does not accept it")
	}

	// openai supports all of them.
	body = bodyOf(t, profile("openai"), req)
	if body["parallel_tool_calls"] != true {
		t.Error("parallel_tool_calls was dropped for a provider that accepts it")
	}
	fn = body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["strict"] != true {
		t.Error("strict was dropped for a provider that accepts it")
	}
}

func TestApplyClampsStopSequences(t *testing.T) {
	req := userReq("m")
	req.Stop = []string{"a", "b", "c", "d", "e", "f"}

	body := bodyOf(t, profile("openai"), req)
	if got := len(body["stop"].([]any)); got != 4 {
		t.Errorf("sent %d stop sequences, want them clamped to 4", got)
	}
}

func TestApplyNeverMutatesTheCallersRequest(t *testing.T) {
	// The agent loop holds the running history and re-sends it every turn. A
	// transform that edited in place would compound: a system message renamed
	// on turn one would be renamed again on turn two, and folding would
	// duplicate the text once per turn.
	temp := 0.5
	req := &oai.ChatCompletionRequest{
		Model:       "o3",
		Temperature: &temp,
		Messages: []oai.Message{
			{Role: oai.RoleSystem, Content: oai.TextContent("be terse")},
			{Role: oai.RoleUser, Content: oai.TextContent("hi")},
		},
	}

	for range 3 {
		Apply(profile("openai-reasoning"), req)
	}

	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Error("the caller's temperature was cleared")
	}
	if req.Messages[0].Role != oai.RoleSystem {
		t.Errorf("the caller's system role was rewritten to %q", req.Messages[0].Role)
	}
	if len(req.Messages) != 2 {
		t.Errorf("the caller's message list grew to %d", len(req.Messages))
	}
}

func TestStripThinkMovesReasoningOutOfContent(t *testing.T) {
	// Left alone the scratchpad reaches the end user, and worse, it goes back
	// upstream next turn where the model treats its own discarded thinking as
	// settled fact.
	resp := &oai.ChatCompletionResponse{Choices: []oai.Choice{{
		Message: oai.Message{
			Role: oai.RoleAssistant,
			Content: oai.TextContent(
				"<think>The user wants X. Maybe Y? No, X.</think>\nThe answer is X."),
		},
	}}}

	StripThink(profile("vllm"), resp)

	msg := resp.Choices[0].Message
	if got := msg.Content.String(); got != "The answer is X." {
		t.Errorf("content = %q", got)
	}
	if !strings.Contains(msg.ReasoningContent, "The user wants X") {
		t.Errorf("the reasoning was discarded rather than moved: %q", msg.ReasoningContent)
	}
	if strings.Contains(msg.ReasoningContent, "<think>") {
		t.Errorf("the tag markers were kept: %q", msg.ReasoningContent)
	}
}

func TestStripThinkOnlyWhenTheProfileAsksForIt(t *testing.T) {
	text := "<think>hidden</think>visible"
	resp := &oai.ChatCompletionResponse{Choices: []oai.Choice{{
		Message: oai.Message{Content: oai.TextContent(text)},
	}}}

	StripThink(profile("openai"), resp)

	if resp.Choices[0].Message.Content.String() != text {
		t.Error("content was altered for a provider that does not emit think tags")
	}
}

func TestStripThinkLeavesOrdinaryContentAlone(t *testing.T) {
	resp := &oai.ChatCompletionResponse{Choices: []oai.Choice{{
		Message: oai.Message{Content: oai.TextContent("no tags here")},
	}}}

	StripThink(profile("vllm"), resp)

	if got := resp.Choices[0].Message.Content.String(); got != "no tags here" {
		t.Errorf("content = %q", got)
	}
}

func TestInferFixRecognisesRealProviderComplaints(t *testing.T) {
	// Phrasings collected from provider error bodies. The point of the layer
	// is that a wrong profile becomes a slower success plus a log line, rather
	// than a 400 about a parameter the caller never set.
	cases := []struct {
		body string
		want Fix
	}{
		{`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`,
			"max_completion_tokens"},
		{`{"error":{"message":"max_tokens is not supported; use max_completion_tokens"}}`,
			"max_completion_tokens"},
		{`{"error":{"message":"unknown field 'max_completion_tokens'"}}`, "max_tokens"},
		{`{"error":{"message":"'developer' is not a valid role"}}`, "system-role"},
		{`{"error":{"message":"'system' is not a valid role for this model"}}`, "developer-role"},
		{`{"error":{"message":"Extra inputs are not permitted: parallel_tool_calls"}}`,
			"no-parallel-tool-calls"},
		{`{"error":{"message":"This model does not support temperature"}}`, "no-sampling"},
		{`{"error":{"message":"unknown parameter 'strict' in function definition"}}`, "no-strict"},
		{`{"error":{"message":"unsupported schema keyword: oneOf"}}`, "basic-schema"},
		{`{"error":{"message":"Extra inputs are not permitted: stream_options"}}`,
			"no-stream-usage"},
	}

	for _, c := range cases {
		fix, ok := inferFix(c.body)
		if !ok {
			t.Errorf("no fix inferred from: %s", c.body)
			continue
		}
		if fix.Name != c.want {
			t.Errorf("inferred %q, want %q, from: %s", fix.Name, c.want, c.body)
		}
	}
}

func TestInferFixIgnoresUnrelatedErrors(t *testing.T) {
	// Guessing at an unrelated 400 would apply a wrong adjustment and keep it
	// for the rest of the process.
	for _, body := range []string{
		`{"error":{"message":"You exceeded your current quota"}}`,
		`{"error":{"message":"invalid api key"}}`,
		`{"error":{"message":"messages: at least one message is required"}}`,
		`{"error":{"message":"model 'gpt-9' does not exist"}}`,
		``,
	} {
		if fix, ok := inferFix(body); ok {
			t.Errorf("inferred %q from an unrelated error: %s", fix.Name, body)
		}
	}
}

func TestProfileOverridesNeedNoCode(t *testing.T) {
	// A provider with no built-in profile has to be reachable from
	// configuration alone, or this layer is a list someone has to maintain.
	cfg := config.Default()
	cfg.Upstream.Profile = "generic"
	cfg.Upstream.ProfileOverrides = json.RawMessage(
		`{"max_tokens_field":"max_completion_tokens","system_role":"developer","sampling":false}`)

	p, err := cfg.ResolveProfile()
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxTokensField != "max_completion_tokens" || p.SystemRole != "developer" || p.Sampling {
		t.Errorf("overrides were not applied: %+v", p)
	}
	// Untouched fields keep the base profile's values.
	if p.SchemaDialect != config.DialectBasic {
		t.Errorf("SchemaDialect = %q, want the base profile's value", p.SchemaDialect)
	}
	if !strings.Contains(p.Name, "overrides") {
		t.Errorf("Name = %q, want it to record that overrides were applied", p.Name)
	}
}

func TestUnknownProfileIsRejectedAtStartup(t *testing.T) {
	// A bad profile name should refuse to boot rather than fail on whichever
	// request happens to arrive first.
	cfg := config.Default()
	cfg.Upstream.Profile = "not-a-provider"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unknown profile was accepted")
	}
	if !strings.Contains(err.Error(), "not-a-provider") {
		t.Errorf("the error does not name the bad profile: %v", err)
	}
	// ...and it should say what the options are.
	if !strings.Contains(err.Error(), "generic") {
		t.Errorf("the error does not list the available profiles: %v", err)
	}
}

func TestEveryBuiltinProfileIsCoherent(t *testing.T) {
	for _, name := range config.ProfileNames() {
		p, _ := config.LookupProfile(name)

		if p.Name != name {
			t.Errorf("%s: Name = %q", name, p.Name)
		}
		switch p.MaxTokensField {
		case "max_tokens", "max_completion_tokens":
		default:
			t.Errorf("%s: MaxTokensField = %q", name, p.MaxTokensField)
		}
		switch p.SystemRole {
		case oai.RoleSystem, oai.RoleDeveloper, oai.RoleUser:
		default:
			t.Errorf("%s: SystemRole = %q", name, p.SystemRole)
		}
		switch p.SchemaDialect {
		case config.DialectFull, config.DialectBasic:
		default:
			t.Errorf("%s: SchemaDialect = %q", name, p.SchemaDialect)
		}
		// Every profile must survive a round trip through the transform.
		if got := Apply(p, userReq("m")); got == nil || len(got.Messages) == 0 {
			t.Errorf("%s: Apply produced an unusable request", name)
		}
	}
}

// rejectingProvider refuses the first request with a given complaint, then
// records whatever arrives next.
type rejectingProvider struct {
	mu        sync.Mutex
	complaint string
	requests  []map[string]any
}

func (p *rejectingProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var decoded map[string]any
	json.Unmarshal(body, &decoded)

	p.mu.Lock()
	p.requests = append(p.requests, decoded)
	n := len(p.requests)
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if n == 1 && p.complaint != "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": p.complaint},
		})
		return
	}
	json.NewEncoder(w).Encode(oai.ChatCompletionResponse{
		ID: "c", Object: "chat.completion", Model: "m",
		Choices: []oai.Choice{{Index: 0, Message: oai.Message{
			Role: oai.RoleAssistant, Content: oai.TextContent("ok"),
		}}},
	})
}

func newClient(t *testing.T, h http.Handler, profileName string) (*Client, *rejectingProvider) {
	t.Helper()
	p, _ := h.(*rejectingProvider)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return NewWithProfile(config.Upstream{
		BaseURL: srv.URL, APIKey: "k", Model: "m",
		Timeout: config.Duration(10 * time.Second), MaxRetries: 0,
	}, profile(profileName), slog.New(slog.NewTextHandler(io.Discard, nil))), p
}

func TestAutodetectLearnsFromARejectionAndRetries(t *testing.T) {
	// The behaviour the whole layer exists for: a wrong profile becomes a
	// slower success instead of a 400 about a parameter the caller never set.
	provider := &rejectingProvider{
		complaint: "Unsupported parameter: 'max_tokens' is not supported with this " +
			"model. Use 'max_completion_tokens' instead.",
	}
	c, _ := newClient(t, provider, "generic")

	limit := 100
	req := userReq("m")
	req.MaxTokens = &limit

	resp, err := c.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("the request was not recovered: %v", err)
	}
	if resp.Choices[0].Message.Content.String() != "ok" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content.String())
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 2 {
		t.Fatalf("provider saw %d requests, want the original plus one retry", len(provider.requests))
	}
	if _, present := provider.requests[0]["max_tokens"]; !present {
		t.Error("the first attempt should have used the configured field")
	}
	if provider.requests[1]["max_completion_tokens"] != float64(100) {
		t.Errorf("the retry did not use the inferred field: %v", provider.requests[1])
	}
}

func TestAutodetectIsRememberedForLaterRequests(t *testing.T) {
	// Paying a failed request for the same lesson on every call would defeat
	// the point.
	provider := &rejectingProvider{
		complaint: "'system' is not a valid role for this model",
	}
	c, _ := newClient(t, provider, "generic")

	for range 3 {
		if _, err := c.Complete(context.Background(), userReq("m")); err != nil {
			t.Fatal(err)
		}
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	// One rejection, one retry, then two clean requests.
	if len(provider.requests) != 4 {
		t.Errorf("provider saw %d requests, want 4 — the lesson was not remembered",
			len(provider.requests))
	}
	if got := c.Profile("m").SystemRole; got != oai.RoleDeveloper {
		t.Errorf("learned SystemRole = %q, want developer", got)
	}
}

func TestAutodetectGivesUpOnAnUninferableRejection(t *testing.T) {
	// Without a stopping condition this is an unbounded retry loop that looks
	// like a hang and costs money on every attempt.
	provider := &rejectingProvider{complaint: "You exceeded your current quota"}
	c, _ := newClient(t, provider, "generic")

	_, err := c.Complete(context.Background(), userReq("m"))
	if err == nil {
		t.Fatal("an unrecoverable rejection was reported as success")
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) != 1 {
		t.Errorf("provider saw %d requests, want exactly 1 with no retry", len(provider.requests))
	}
}

func TestAutodetectDoesNotAdoptTheSameFixTwice(t *testing.T) {
	// A provider that keeps repeating one complaint must not loop.
	alwaysComplains := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
			"message": "Unsupported parameter: 'max_tokens'. Use 'max_completion_tokens'.",
		}})
	})

	var count atomic.Int32
	counted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		alwaysComplains.ServeHTTP(w, r)
	})

	c, _ := newClient(t, counted, "generic")
	if _, err := c.Complete(context.Background(), userReq("m")); err == nil {
		t.Fatal("expected the request to fail")
	}
	if n := count.Load(); n != 2 {
		t.Errorf("provider saw %d requests, want 2 — one retry, then give up", n)
	}
}

func TestAutodetectIsPerModel(t *testing.T) {
	// Two models behind one endpoint can have different rules, and applying
	// one model's lesson to another would be a fresh source of failures.
	provider := &rejectingProvider{complaint: "This model does not support temperature"}
	c, _ := newClient(t, provider, "openai")

	temp := 0.7
	req := userReq("reasoning-model")
	req.Temperature = &temp
	if _, err := c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if c.Profile("reasoning-model").Sampling {
		t.Error("the lesson was not recorded for the model that taught it")
	}
	if !c.Profile("other-model").Sampling {
		t.Error("one model's lesson was applied to another")
	}
}
