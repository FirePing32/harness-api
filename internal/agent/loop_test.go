package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/guard"
	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/tools"
	"github.com/FirePing32/harness-api/internal/upstream"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// scriptedProvider replays a fixed sequence of assistant turns, recording what
// it was sent. Standing in for a model keeps the loop's behaviour deterministic
// — the thing under test is the orchestration, not the model.
type scriptedProvider struct {
	mu       sync.Mutex
	turns    []oai.Message
	received []oai.ChatCompletionRequest
	calls    atomic.Int32
}

func (p *scriptedProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req oai.ChatCompletionRequest
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	p.mu.Lock()
	p.received = append(p.received, req)
	n := int(p.calls.Add(1)) - 1
	var msg oai.Message
	if n < len(p.turns) {
		msg = p.turns[n]
	} else {
		msg = assistantText("ran out of scripted turns")
	}
	p.mu.Unlock()

	finish := oai.FinishStop
	if len(msg.ToolCalls) > 0 {
		finish = oai.FinishToolCalls
	}
	resp := oai.ChatCompletionResponse{
		ID: "chatcmpl-test", Object: "chat.completion", Model: "test-model",
		Choices: []oai.Choice{{Index: 0, Message: msg, FinishReason: &finish}},
		Usage:   &oai.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (p *scriptedProvider) lastRequest(t *testing.T) oai.ChatCompletionRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.received) == 0 {
		t.Fatal("the provider was never called")
	}
	return p.received[len(p.received)-1]
}

func assistantText(s string) oai.Message {
	return oai.Message{Role: oai.RoleAssistant, Content: oai.TextContent(s)}
}

func assistantTools(calls ...oai.ToolCall) oai.Message {
	return oai.Message{Role: oai.RoleAssistant, Content: oai.NullContent(), ToolCalls: calls}
}

func toolCall(id, name string, args map[string]any) oai.ToolCall {
	raw, _ := json.Marshal(args)
	return oai.ToolCall{
		ID: id, Type: oai.ToolTypeFunction,
		Function: oai.FunctionCall{Name: name, Arguments: string(raw)},
	}
}

// harness wires a loop, a scripted provider, and a seeded workspace together.
type harness struct {
	loop     *Loop
	provider *scriptedProvider
	session  *workspace.Session
}

func newHarness(t *testing.T, files map[string]string, turns []oai.Message, cfg ...config.Agent) *harness {
	t.Helper()

	provider := &scriptedProvider{turns: turns}
	srv := httptest.NewServer(provider)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	m, err := workspace.NewManager(config.Workspace{
		Root: t.TempDir(), IdleTTL: config.Duration(time.Hour),
	}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })

	s, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		full := filepath.Join(s.Root(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s.ReloadIgnore()

	agentCfg := config.Default().Agent
	if len(cfg) > 0 {
		agentCfg = cfg[0]
	}

	client := upstream.New(config.Upstream{
		BaseURL: srv.URL, APIKey: "test-key", Model: "test-model",
		Timeout: config.Duration(30 * time.Second), MaxRetries: 0,
	}, log)

	return &harness{
		loop: New(Options{
			Upstream: client,
			Registry: tools.NewRegistry(tools.NewRead(), tools.NewGlob(),
				tools.NewGrep(), tools.NewWrite(), tools.NewEdit()),
			Config: agentCfg,
			Log:    log,
		}),
		provider: provider,
		session:  s,
	}
}

func (h *harness) run(t *testing.T, prompt string) *Result {
	t.Helper()
	h.session.Lock()
	defer h.session.Unlock()

	res, err := h.loop.Run(context.Background(), h.session, &oai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent(prompt)}},
	})
	if err != nil {
		t.Fatalf("loop failed: %v", err)
	}
	return res
}

func (h *harness) fileContent(t *testing.T, rel string) string {
	t.Helper()
	b, err := h.session.Jail().ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLoopAnswersWithoutTools(t *testing.T) {
	h := newHarness(t, nil, []oai.Message{assistantText("Two.")})

	res := h.run(t, "what is one plus one")

	if res.Stop != StopComplete {
		t.Errorf("Stop = %q, want complete", res.Stop)
	}
	if res.Turns != 1 {
		t.Errorf("Turns = %d, want 1", res.Turns)
	}
	if got := res.Final.Content.String(); got != "Two." {
		t.Errorf("Final = %q", got)
	}
}

func TestLoopReadsAFileThenAnswers(t *testing.T) {
	h := newHarness(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n"},
		[]oai.Message{
			assistantTools(toolCall("c1", "read", map[string]any{"path": "main.go"})),
			assistantText("It is an empty main package."),
		})

	res := h.run(t, "read main.go and explain it")

	if res.Stop != StopComplete || res.Turns != 2 {
		t.Fatalf("Stop=%q Turns=%d", res.Stop, res.Turns)
	}

	// The tool result has to reach the model, and it has to carry the contents.
	sent := h.provider.lastRequest(t)
	var toolMsg *oai.Message
	for i := range sent.Messages {
		if sent.Messages[i].Role == oai.RoleTool {
			toolMsg = &sent.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool message was sent back to the model")
	}
	if !strings.Contains(toolMsg.Content.String(), "func main()") {
		t.Errorf("the tool result does not carry the file contents: %q", toolMsg.Content.String())
	}
	if toolMsg.ToolCallID != "c1" {
		t.Errorf("ToolCallID = %q, want c1", toolMsg.ToolCallID)
	}
}

func TestLoopEditsFilesEndToEnd(t *testing.T) {
	// The milestone case: the model renames a function and fixes the call site,
	// and the files on disk actually change.
	h := newHarness(t, map[string]string{
		"main.go": "package main\n\nfunc main() {\n\toldName()\n}\n",
		"util.go": "package main\n\nfunc oldName() {\n\tprintln(\"hi\")\n}\n",
	}, []oai.Message{
		assistantTools(
			toolCall("c1", "read", map[string]any{"path": "main.go"}),
			toolCall("c2", "read", map[string]any{"path": "util.go"}),
		),
		assistantTools(toolCall("c3", "edit", map[string]any{
			"path": "util.go", "old_string": "func oldName()", "new_string": "func newName()",
		})),
		assistantTools(toolCall("c4", "edit", map[string]any{
			"path": "main.go", "old_string": "oldName()", "new_string": "newName()",
		})),
		assistantText("Renamed oldName to newName and updated the call site."),
	})

	res := h.run(t, "rename oldName to newName and fix the call sites")

	if res.Stop != StopComplete {
		t.Fatalf("Stop = %q", res.Stop)
	}

	if got := h.fileContent(t, "util.go"); !strings.Contains(got, "func newName()") {
		t.Errorf("util.go was not edited: %q", got)
	}
	if got := h.fileContent(t, "main.go"); !strings.Contains(got, "newName()") {
		t.Errorf("main.go was not edited: %q", got)
	}
	if got := h.fileContent(t, "main.go"); strings.Contains(got, "oldName") {
		t.Errorf("the old name survives in main.go: %q", got)
	}
}

func TestLoopSurfacesToolErrorsToTheModelRatherThanFailing(t *testing.T) {
	// A failed tool call is a normal turn. Ending the request at the moment the
	// model was about to correct itself is the worst possible response.
	h := newHarness(t, map[string]string{"a.go": "package main\n"}, []oai.Message{
		assistantTools(toolCall("c1", "read", map[string]any{"path": "missing.go"})),
		assistantTools(toolCall("c2", "read", map[string]any{"path": "a.go"})),
		assistantText("Found it in a.go."),
	})

	res := h.run(t, "read the file")

	if res.Stop != StopComplete {
		t.Fatalf("Stop = %q, want the loop to recover", res.Stop)
	}
	sent := h.provider.lastRequest(t)
	found := false
	for _, m := range sent.Messages {
		if m.Role == oai.RoleTool && strings.HasPrefix(m.Content.String(), "Error:") {
			found = true
		}
	}
	if !found {
		t.Error("the tool failure was not marked as an error in the message sent back")
	}
}

func TestLoopStopsAtTheIterationCeiling(t *testing.T) {
	// A model that never stops calling tools is a billing incident, not a hang.
	cfg := config.Default().Agent
	cfg.MaxIterations = 3

	var turns []oai.Message
	for range 20 {
		turns = append(turns, assistantTools(
			toolCall("c", "glob", map[string]any{"pattern": "**/*"})))
	}
	h := newHarness(t, map[string]string{"a.go": "x\n"}, turns, cfg)

	res := h.run(t, "loop forever")

	if res.Stop != StopMaxIterations {
		t.Fatalf("Stop = %q, want max_iterations", res.Stop)
	}
	if res.Turns != 3 {
		t.Errorf("Turns = %d, want the configured ceiling of 3", res.Turns)
	}
	if got := int(h.provider.calls.Load()); got != 3 {
		t.Errorf("the provider was called %d times, want 3", got)
	}
	// finish_reason "length" is the only standard value meaning "stopped early".
	if got := res.Stop.FinishReason(); got != oai.FinishLength {
		t.Errorf("FinishReason = %q, want length", got)
	}
	// And the content must say so, because plenty of clients ignore the flag.
	if !strings.Contains(res.Final.Content.String(), "Stopped after") {
		t.Errorf("a truncated answer does not say it was truncated: %q",
			res.Final.Content.String())
	}
}

func TestLoopAggregatesUsageAcrossEveryUpstreamCall(t *testing.T) {
	// The client asked one question; this is what answering it cost. Reporting
	// only the final turn would understate a long run by an order of magnitude.
	h := newHarness(t, map[string]string{"a.go": "x\n"}, []oai.Message{
		assistantTools(toolCall("c1", "read", map[string]any{"path": "a.go"})),
		assistantTools(toolCall("c2", "glob", map[string]any{"pattern": "*"})),
		assistantText("done"),
	})

	res := h.run(t, "look around")

	if res.Turns != 3 {
		t.Fatalf("Turns = %d", res.Turns)
	}
	if res.Usage.TotalTokens != 45 {
		t.Errorf("TotalTokens = %d, want 45 (3 calls x 15)", res.Usage.TotalTokens)
	}
	if res.Usage.PromptTokens != 30 {
		t.Errorf("PromptTokens = %d, want 30", res.Usage.PromptTokens)
	}
}

func TestLoopRunsConcurrencySafeCallsTogetherAndKeepsOrder(t *testing.T) {
	// Results must come back in the order the model asked for them: strict
	// providers reject a request whose tool results do not correspond
	// positionally to the preceding tool_calls array.
	h := newHarness(t, map[string]string{
		"a.go": "AAA\n", "b.go": "BBB\n", "c.go": "CCC\n",
	}, []oai.Message{
		assistantTools(
			toolCall("c1", "read", map[string]any{"path": "a.go"}),
			toolCall("c2", "read", map[string]any{"path": "b.go"}),
			toolCall("c3", "read", map[string]any{"path": "c.go"}),
		),
		assistantText("read all three"),
	})

	h.run(t, "read all three files")

	sent := h.provider.lastRequest(t)
	var ids []string
	for _, m := range sent.Messages {
		if m.Role == oai.RoleTool {
			ids = append(ids, m.ToolCallID)
		}
	}
	want := []string{"c1", "c2", "c3"}
	if len(ids) != 3 {
		t.Fatalf("got %d tool results, want 3", len(ids))
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("tool result order = %v, want %v", ids, want)
			break
		}
	}
}

func TestLoopRunsWritesAloneAsBarriers(t *testing.T) {
	// A write between two reads must not overlap either of them, or the reads
	// see a torn view. Correctness here is observable through the ledger: the
	// second read has to see what the write produced.
	h := newHarness(t, map[string]string{"a.go": "before\n"}, []oai.Message{
		assistantTools(toolCall("c0", "read", map[string]any{"path": "a.go"})),
		assistantTools(
			toolCall("c1", "read", map[string]any{"path": "a.go"}),
			toolCall("c2", "write", map[string]any{"path": "a.go", "content": "after\n"}),
			toolCall("c3", "read", map[string]any{"path": "a.go"}),
		),
		assistantText("done"),
	})

	h.run(t, "read, write, read")

	sent := h.provider.lastRequest(t)
	var results []string
	for _, m := range sent.Messages {
		if m.Role == oai.RoleTool {
			results = append(results, m.Content.String())
		}
	}
	if len(results) != 4 {
		t.Fatalf("got %d tool results, want 4", len(results))
	}
	if !strings.Contains(results[1], "before") {
		t.Errorf("the read before the write saw the wrong contents: %q", results[1])
	}
	if !strings.Contains(results[3], "after") {
		t.Errorf("the read after the write did not observe it: %q", results[3])
	}
}

func TestLoopSendsToolDefinitionsAndASystemPrompt(t *testing.T) {
	h := newHarness(t, nil, []oai.Message{assistantText("hi")})

	h.run(t, "hello")

	sent := h.provider.lastRequest(t)
	if len(sent.Tools) != 5 {
		t.Errorf("sent %d tool definitions, want 5", len(sent.Tools))
	}
	if sent.Messages[0].Role != oai.RoleSystem {
		t.Fatalf("first message role = %q, want system", sent.Messages[0].Role)
	}
	prompt := sent.Messages[0].Content.String()
	if !strings.Contains(prompt, h.session.Root()) {
		t.Error("the system prompt does not name the workspace root")
	}
	if !strings.Contains(prompt, "read") {
		t.Error("the system prompt does not list the available tools")
	}
}

func TestLoopMergesACallerSystemPromptLast(t *testing.T) {
	// A caller that sets a system prompt expects it to win where it conflicts,
	// but it must still be legible as the caller's text rather than ours.
	h := newHarness(t, nil, []oai.Message{assistantText("ok")})
	h.session.Lock()
	defer h.session.Unlock()

	_, err := h.loop.Run(context.Background(), h.session, &oai.ChatCompletionRequest{
		Model: "test-model",
		Messages: []oai.Message{
			{Role: oai.RoleSystem, Content: oai.TextContent("Always reply in French.")},
			{Role: oai.RoleUser, Content: oai.TextContent("hello")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sent := h.provider.lastRequest(t)
	prompt := sent.Messages[0].Content.String()
	if !strings.Contains(prompt, "Always reply in French.") {
		t.Fatal("the caller's system message was dropped")
	}
	if !strings.Contains(prompt, "Instructions from the user") {
		t.Error("the caller's text is not marked as theirs")
	}
	if idx := strings.Index(prompt, "Always reply in French."); idx < len(prompt)/2 {
		t.Error("the caller's instructions should come last, where they take precedence")
	}
	// The caller's system message must not also appear as a separate turn.
	for _, m := range sent.Messages[1:] {
		if m.Role == oai.RoleSystem {
			t.Error("the system message was both merged and passed through")
		}
	}
}

func TestLoopHonoursAToolAllowlist(t *testing.T) {
	h := newHarness(t, nil, []oai.Message{assistantText("ok")})
	h.session.Lock()
	defer h.session.Unlock()

	_, err := h.loop.Run(context.Background(), h.session, &oai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
		Harness:  &oai.HarnessRequestExt{Tools: []string{"read", "glob"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	sent := h.provider.lastRequest(t)
	if len(sent.Tools) != 2 {
		t.Fatalf("sent %d tools, want the 2 allowed", len(sent.Tools))
	}
	for _, tool := range sent.Tools {
		if tool.Function.Name != "read" && tool.Function.Name != "glob" {
			t.Errorf("%q was offered despite the allowlist", tool.Function.Name)
		}
	}
}

func TestLoopRejectsAnUnknownToolInTheAllowlist(t *testing.T) {
	h := newHarness(t, nil, []oai.Message{assistantText("ok")})
	h.session.Lock()
	defer h.session.Unlock()

	_, err := h.loop.Run(context.Background(), h.session, &oai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
		Harness:  &oai.HarnessRequestExt{Tools: []string{"nonexistent"}},
	})
	if err == nil {
		t.Fatal("an unknown tool name in the allowlist was accepted")
	}
	var apiErr *oai.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest {
		t.Errorf("err = %v, want a 400 APIError", err)
	}
}

func TestLoopRequestMayLowerButNotRaiseTheIterationCap(t *testing.T) {
	// The caller is not the one paying the upstream bill.
	cfg := config.Default().Agent
	cfg.MaxIterations = 5

	base := BudgetFrom(cfg)
	if got := base.WithRequestOverride(ptr(2)).MaxIterations; got != 2 {
		t.Errorf("lowering to 2 gave %d", got)
	}
	if got := base.WithRequestOverride(ptr(500)).MaxIterations; got != 5 {
		t.Errorf("raising to 500 gave %d, want the server ceiling of 5", got)
	}
	if got := base.WithRequestOverride(ptr(0)).MaxIterations; got != 5 {
		t.Errorf("a zero override gave %d, want the server ceiling", got)
	}
	if got := base.WithRequestOverride(nil).MaxIterations; got != 5 {
		t.Errorf("a nil override gave %d", got)
	}
}

func TestLoopStopsOnCancellation(t *testing.T) {
	h := newHarness(t, nil, []oai.Message{assistantText("never reached")})
	h.session.Lock()
	defer h.session.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := h.loop.Run(ctx, h.session, &oai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
	})
	if err != nil {
		t.Fatalf("a cancelled run should return a result, not an error: %v", err)
	}
	if res.Stop != StopCancelled {
		t.Errorf("Stop = %q, want cancelled", res.Stop)
	}
	if h.provider.calls.Load() != 0 {
		t.Error("the provider was called despite a cancelled context")
	}
}

func TestLoopRequiresANonSystemMessage(t *testing.T) {
	h := newHarness(t, nil, []oai.Message{assistantText("ok")})
	h.session.Lock()
	defer h.session.Unlock()

	_, err := h.loop.Run(context.Background(), h.session, &oai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []oai.Message{{Role: oai.RoleSystem, Content: oai.TextContent("be helpful")}},
	})
	if err == nil {
		t.Fatal("a conversation with only a system message was accepted")
	}
}

func TestLoopNeverSendsHarnessExtensionUpstream(t *testing.T) {
	h := newHarness(t, nil, []oai.Message{assistantText("ok")})
	h.session.Lock()
	defer h.session.Unlock()

	_, err := h.loop.Run(context.Background(), h.session, &oai.ChatCompletionRequest{
		Model:    "test-model",
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}},
		Harness:  &oai.HarnessRequestExt{SessionID: "ws_secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sent := h.provider.lastRequest(t); sent.Harness != nil {
		t.Error("the harness extension leaked to the provider")
	}
}

func TestLoopToResponse(t *testing.T) {
	h := newHarness(t, nil, []oai.Message{assistantText("the answer")})
	res := h.run(t, "ask")

	resp := res.ToResponse("chatcmpl-x", "my-model", 1700000000)
	if resp.Object != "chat.completion" {
		t.Errorf("Object = %q", resp.Object)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("got %d choices", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content.String() != "the answer" {
		t.Errorf("content = %q", resp.Choices[0].Message.Content.String())
	}
	if *resp.Choices[0].FinishReason != oai.FinishStop {
		t.Errorf("FinishReason = %q", *resp.Choices[0].FinishReason)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 15 {
		t.Errorf("Usage = %+v", resp.Usage)
	}
}

func TestSplitSystemExtractsFromAnywhere(t *testing.T) {
	// Clients replaying a conversation sometimes interleave system messages, and
	// most providers treat a mid-history system message as an instruction rather
	// than as dialogue.
	system, rest := SplitSystem([]oai.Message{
		{Role: oai.RoleUser, Content: oai.TextContent("hi")},
		{Role: oai.RoleSystem, Content: oai.TextContent("be terse")},
		{Role: oai.RoleAssistant, Content: oai.TextContent("hello")},
		{Role: oai.RoleDeveloper, Content: oai.TextContent("use Go")},
	})

	if len(system) != 2 {
		t.Fatalf("extracted %d system messages, want 2", len(system))
	}
	if len(rest) != 2 {
		t.Fatalf("kept %d conversation messages, want 2", len(rest))
	}
	for _, m := range rest {
		if m.Role == oai.RoleSystem || m.Role == oai.RoleDeveloper {
			t.Error("a system message survived in the conversation")
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestLoopGuardDenialReachesTheModelAsAToolResult(t *testing.T) {
	// A denial is a normal turn, not a transport error. Ending the request
	// here would stop it at exactly the moment the model was about to change
	// approach — which is the whole point of denying rather than allowing.
	h := newHarness(t, map[string]string{"a.txt": "x\n"}, []oai.Message{
		assistantTools(toolCall("c1", "read", map[string]any{"path": "a.txt"})),
		assistantText("understood, stopping"),
	})
	h.loop.guards = guard.NewChain(
		guard.New("test-deny", func(guard.Execution) string {
			return "denied for testing; do something else"
		}),
	)

	res := h.run(t, "read it")

	if res.Stop != StopComplete {
		t.Fatalf("Stop = %q, want the run to continue after a denial", res.Stop)
	}

	sent := h.provider.lastRequest(t)
	var toolMsg string
	for _, m := range sent.Messages {
		if m.Role == oai.RoleTool {
			toolMsg = m.Content.String()
		}
	}
	if !strings.Contains(toolMsg, "denied for testing") {
		t.Errorf("the denial did not reach the model: %q", toolMsg)
	}
	if !strings.HasPrefix(toolMsg, "Error:") {
		t.Errorf("a denial must be marked as an error result: %q", toolMsg)
	}
}

func TestLoopGuardStopsTheToolFromRunning(t *testing.T) {
	// The denial has to prevent the side effect, not merely report on it.
	h := newHarness(t, map[string]string{"a.txt": "original\n"}, []oai.Message{
		assistantTools(toolCall("c1", "write", map[string]any{
			"path": "a.txt", "content": "overwritten\n",
		})),
		assistantText("ok"),
	})
	h.loop.guards = guard.NewChain(
		guard.New("no-writes", func(ex guard.Execution) string {
			if ex.Tool == "write" {
				return "writes are not permitted in this request"
			}
			return ""
		}),
	)

	h.run(t, "overwrite it")

	if got := h.fileContent(t, "a.txt"); got != "original\n" {
		t.Errorf("the denied write still happened: %q", got)
	}
}

func TestLoopGuardSeesPriorCallsAcrossTurns(t *testing.T) {
	// Loop detection is only possible from the request's history; a single
	// call in isolation never looks wrong.
	h := newHarness(t, map[string]string{"a.txt": "x\n"}, []oai.Message{
		assistantTools(toolCall("c1", "read", map[string]any{"path": "a.txt"})),
		assistantTools(toolCall("c2", "read", map[string]any{"path": "a.txt"})),
		assistantTools(toolCall("c3", "read", map[string]any{"path": "a.txt"})),
		assistantText("giving up"),
	})

	var seen []int
	h.loop.guards = guard.NewChain(
		guard.New("observer", func(ex guard.Execution) string {
			seen = append(seen, len(ex.Prior))
			return ""
		}),
	)

	h.run(t, "read it repeatedly")

	want := []int{0, 1, 2}
	if len(seen) != len(want) {
		t.Fatalf("guard saw %d calls, want %d", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("call %d saw %d prior calls, want %d", i, seen[i], want[i])
		}
	}
}

func TestLoopRepeatGuardBreaksAnIdenticalCallLoop(t *testing.T) {
	// End to end with the real guard: a model stuck repeating one call is
	// stopped well before the iteration ceiling burns the whole budget.
	var turns []oai.Message
	for range 10 {
		turns = append(turns, assistantTools(
			toolCall("c", "read", map[string]any{"path": "a.txt"})))
	}
	turns = append(turns, assistantText("stopped repeating"))

	h := newHarness(t, map[string]string{"a.txt": "x\n"}, turns)
	h.loop.guards = guard.NewChain(guard.RepeatTool(3))

	h.run(t, "read it")

	sent := h.provider.lastRequest(t)
	var denials int
	for _, m := range sent.Messages {
		if m.Role == oai.RoleTool && strings.Contains(m.Content.String(), "cannot make progress") {
			denials++
		}
	}
	if denials == 0 {
		t.Error("the repeat guard never fired on an identical-call loop")
	}
}
