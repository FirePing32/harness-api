package contextmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/FirePing32/harness-api/internal/oai"
)

// stubSummarizer returns a fixed summary and records what it was asked to
// summarise.
type stubSummarizer struct {
	summary string
	err     error
	sawText string
	calls   int
}

func (s *stubSummarizer) Complete(
	_ context.Context, req *oai.ChatCompletionRequest,
) (*oai.ChatCompletionResponse, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	for _, m := range req.Messages {
		if m.Role == oai.RoleUser {
			s.sawText = m.Content.String()
		}
	}
	return &oai.ChatCompletionResponse{Choices: []oai.Choice{{
		Message: oai.Message{Role: oai.RoleAssistant, Content: oai.TextContent(s.summary)},
	}}}, nil
}

func newCompactor(t *testing.T, window int, s Summarizer) *Compactor {
	t.Helper()
	return New(Options{
		Summarizer: s,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Window:     window,
	})
}

// conversation builds a history with n read/result pairs, each result of the
// given size, preceded by a system prompt and a user task.
func conversation(pairs, resultBytes int) []oai.Message {
	msgs := []oai.Message{
		{Role: oai.RoleSystem, Content: oai.TextContent("you are an agent")},
		{Role: oai.RoleUser, Content: oai.TextContent("fix the bug")},
	}
	for i := range pairs {
		id := fmt.Sprintf("c%d", i)
		path := fmt.Sprintf("file%d.go", i)
		msgs = append(msgs,
			oai.Message{
				Role: oai.RoleAssistant, Content: oai.NullContent(),
				ToolCalls: []oai.ToolCall{{
					ID: id, Type: "function",
					Function: oai.FunctionCall{
						Name:      "read",
						Arguments: fmt.Sprintf(`{"path":%q}`, path),
					},
				}},
			},
			oai.Message{
				Role: oai.RoleTool, ToolCallID: id,
				Content: oai.TextContent(strings.Repeat("x", resultBytes)),
			},
		)
	}
	return msgs
}

func TestCompactDoesNothingWellBelowTheThreshold(t *testing.T) {
	c := newCompactor(t, 100_000, &stubSummarizer{summary: "s"})

	res, err := c.Maybe(context.Background(), conversation(3, 100), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Compacted() {
		t.Errorf("compacted a short conversation: %s", res.Method)
	}
}

func TestCompactPrunesToolResultsBeforeSummarising(t *testing.T) {
	// Pruning is free and summarising costs a generation plus detail. Getting
	// the order wrong spends both to solve a problem deleting stale file
	// contents would have solved for nothing.
	sum := &stubSummarizer{summary: "should not be called"}
	c := newCompactor(t, 20_000, sum)

	res, err := c.Maybe(context.Background(), conversation(12, 4000), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodPrune {
		t.Fatalf("Method = %q, want prune", res.Method)
	}
	if sum.calls != 0 {
		t.Error("the summariser was called even though pruning was enough")
	}
	if res.AfterTokens >= res.BeforeTokens {
		t.Errorf("pruning did not shrink the conversation: %d -> %d",
			res.BeforeTokens, res.AfterTokens)
	}
}

func TestCompactReportsThePathsItPruned(t *testing.T) {
	// Those observations have to be invalidated, or the read-before-edit check
	// goes on vouching for bytes nobody can see.
	c := newCompactor(t, 20_000, &stubSummarizer{summary: "s"})

	res, err := c.Maybe(context.Background(), conversation(12, 4000), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.PrunedPaths) == 0 {
		t.Fatal("no pruned paths reported")
	}
	for _, p := range res.PrunedPaths {
		if !strings.HasPrefix(p, "file") {
			t.Errorf("unexpected pruned path %q", p)
		}
	}
}

func TestCompactPrunedStubSaysWhatWasDropped(t *testing.T) {
	// A model that sees an empty tool result concludes the tool is broken. It
	// has to be told the contents were removed and can be fetched again.
	c := newCompactor(t, 20_000, &stubSummarizer{summary: "s"})

	res, err := c.Maybe(context.Background(), conversation(12, 4000), nil)
	if err != nil {
		t.Fatal(err)
	}

	var stub string
	for _, m := range res.Messages {
		if m.Role == oai.RoleTool && strings.Contains(m.Content.String(), "omitted") {
			stub = m.Content.String()
			break
		}
	}
	if stub == "" {
		t.Fatal("no stub found in the pruned conversation")
	}
	if !strings.Contains(stub, "file") {
		t.Errorf("the stub does not name the file: %q", stub)
	}
	if !strings.Contains(stub, "Re-read") {
		t.Errorf("the stub does not say the contents can be fetched again: %q", stub)
	}
}

func TestCompactKeepsRecentMessagesVerbatim(t *testing.T) {
	// The model needs its immediate working state intact; a summary of what it
	// did four seconds ago is worse than useless.
	c := newCompactor(t, 20_000, &stubSummarizer{summary: "s"})
	msgs := conversation(12, 4000)

	res, err := c.Maybe(context.Background(), msgs, nil)
	if err != nil {
		t.Fatal(err)
	}

	last := res.Messages[len(res.Messages)-1]
	if strings.Contains(last.Content.String(), "omitted") {
		t.Error("the most recent tool result was pruned")
	}
}

func TestCompactSummarisesWhenPruningIsNotEnough(t *testing.T) {
	sum := &stubSummarizer{summary: "- edited file3.go\n- still to do: run tests"}
	c := newCompactor(t, 2_000, sum)

	res, err := c.Maybe(context.Background(), conversation(30, 500), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodSummarize {
		t.Fatalf("Method = %q, want summarize", res.Method)
	}
	if sum.calls != 1 {
		t.Errorf("summariser called %d times, want 1", sum.calls)
	}
	if !res.DroppedAll {
		t.Error("DroppedAll should be set when a summary replaced messages")
	}
	if len(res.Messages) >= len(conversation(30, 500)) {
		t.Error("summarising did not shorten the conversation")
	}
}

func TestSummaryIsCarriedInTheConversation(t *testing.T) {
	sum := &stubSummarizer{summary: "- edited file3.go"}
	c := newCompactor(t, 2_000, sum)

	res, err := c.Maybe(context.Background(), conversation(30, 500), nil)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, m := range res.Messages {
		if strings.Contains(m.Content.String(), "edited file3.go") {
			found = true
			if !strings.Contains(m.Content.String(), "Earlier messages were removed") {
				t.Error("the summary is not labelled as a summary, so the model " +
					"cannot tell it from its own earlier output")
			}
		}
	}
	if !found {
		t.Error("the summary did not make it into the conversation")
	}
}

func TestSummariserSeesATranscriptNotItsOwnHistory(t *testing.T) {
	// Handing a model a history as its own history produces a continuation,
	// not a summary.
	sum := &stubSummarizer{summary: "s"}
	c := newCompactor(t, 2_000, sum)

	if _, err := c.Maybe(context.Background(), conversation(30, 500), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sum.sawText, "## assistant") {
		t.Errorf("the transcript was not flattened into a user message: %q",
			firstN(sum.sawText, 200))
	}
}

func TestCompactPreservesTheSystemPromptAndTask(t *testing.T) {
	// The task statement survives verbatim rather than being entrusted to the
	// summariser. If a summary paraphrases it loosely or drops the goal, the
	// agent carries on with no idea what it was asked to do, and the run fails
	// in a way that looks like model incompetence rather than context loss.
	c := newCompactor(t, 2_000, &stubSummarizer{summary: "notes that omit the task"})

	res, err := c.Maybe(context.Background(), conversation(30, 500), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != MethodSummarize {
		t.Fatalf("Method = %q, want summarize", res.Method)
	}

	if res.Messages[0].Role != oai.RoleSystem {
		t.Errorf("the system prompt was dropped; first role = %q", res.Messages[0].Role)
	}
	if res.Messages[1].Role != oai.RoleUser ||
		res.Messages[1].Content.String() != "fix the bug" {
		t.Errorf("the task statement did not survive verbatim: %+v", res.Messages[1])
	}
}

func TestCompactNeverCutsBetweenAToolCallAndItsResult(t *testing.T) {
	// Cutting there leaves an unanswered tool call, and strict providers reject
	// the whole request — which surfaces as a 400 on the turn *after*
	// compaction, with nothing in it pointing at compaction.
	c := newCompactor(t, 2_000, &stubSummarizer{summary: "s"})

	for pairs := 6; pairs < 40; pairs++ {
		res, err := c.Maybe(context.Background(), conversation(pairs, 400), nil)
		if err != nil {
			t.Fatal(err)
		}
		assertToolCallsAnswered(t, res.Messages, pairs)
	}
}

// assertToolCallsAnswered checks that no tool result is orphaned and no tool
// call is left unanswered.
func assertToolCallsAnswered(t *testing.T, msgs []oai.Message, pairs int) {
	t.Helper()

	open := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case oai.RoleAssistant:
			for _, tc := range m.ToolCalls {
				open[tc.ID] = true
			}
		case oai.RoleTool:
			if !open[m.ToolCallID] {
				t.Fatalf("pairs=%d: message %d answers tool call %q that is not in the "+
					"conversation", pairs, i, m.ToolCallID)
			}
			delete(open, m.ToolCallID)
		}
	}
	if len(open) > 0 {
		t.Fatalf("pairs=%d: %d tool call(s) left unanswered after compaction", pairs, len(open))
	}
}

func TestCompactFallsBackToPruningWhenSummarisationFails(t *testing.T) {
	// A failed summary is not a failed request. The pruned conversation is
	// still smaller than what we started with.
	sum := &stubSummarizer{err: errors.New("provider is down")}
	c := newCompactor(t, 2_000, sum)

	res, err := c.Maybe(context.Background(), conversation(30, 500), nil)
	if err != nil {
		t.Fatalf("a failed summary became a failed request: %v", err)
	}
	if res.Method != MethodPrune {
		t.Errorf("Method = %q, want a fallback to prune", res.Method)
	}
}

func TestCompactWithNoWindowDoesNothing(t *testing.T) {
	// Guessing a window is worse than doing nothing: too small wastes a
	// summarisation on every request, too large fails to prevent the overflow.
	c := newCompactor(t, 0, &stubSummarizer{summary: "s"})

	res, err := c.Maybe(context.Background(), conversation(50, 5000), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Compacted() {
		t.Error("compacted without knowing the window size")
	}
}

func TestIsOverflowRecognisesRealProviderMessages(t *testing.T) {
	for _, msg := range []string{
		"This model's maximum context length is 8192 tokens",
		`{"error":{"code":"context_length_exceeded"}}`,
		"Please reduce the length of the messages",
		"input is too long for requested model",
		"prompt is too long: 250000 tokens > 200000 maximum",
		"too many tokens in the request",
	} {
		if !IsOverflow(errors.New(msg)) {
			t.Errorf("not recognised as an overflow: %s", msg)
		}
	}
}

func TestIsOverflowIgnoresUnrelatedErrors(t *testing.T) {
	for _, msg := range []string{
		"You exceeded your current quota",
		"invalid api key",
		"connection refused",
		"rate limit reached",
	} {
		if IsOverflow(errors.New(msg)) {
			t.Errorf("wrongly treated as an overflow: %s", msg)
		}
	}
	if IsOverflow(nil) {
		t.Error("nil is not an overflow")
	}
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
