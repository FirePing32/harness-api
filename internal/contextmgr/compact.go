package contextmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/FirePing32/harness-api/internal/oai"
)

// Three mechanisms, not one, applied cheapest first.
//
// A long agent run overflows the window not because the conversation is long
// but because tool results are large. Twenty file reads at 40 KB each is most
// of a context window, and almost none of it is still needed: the model read
// the file, made its edit, and moved on.
//
// So the first pass drops the bodies of old tool results and keeps everything
// else. It costs nothing, needs no model call, and on a typical run it is
// enough. Only when that fails does the second pass summarise, which costs an
// extra generation and loses detail. The third is recovery: if a request comes
// back saying the context was exceeded anyway, compact harder and retry rather
// than returning an error the caller can do nothing with.
//
// Getting the order wrong — summarising first — spends a model call and
// discards information to solve a problem that deleting stale file contents
// would have solved for free.

const (
	// DefaultThreshold is the fraction of the window at which compaction runs.
	// Matching the DeepSeek Harness default.
	DefaultThreshold = 0.8

	// DefaultRetain is the fraction of the window kept verbatim as the most
	// recent messages. The model needs its immediate working state intact; a
	// summary of what it did four seconds ago is worse than useless.
	DefaultRetain = 0.16

	// prunedResultBytes is the size above which an old tool result is replaced
	// by a stub.
	prunedResultBytes = 2 << 10
)

// Summarizer is the subset of the upstream client compaction needs. An
// interface rather than the concrete client so that summarisation is testable
// without a provider, and so this package does not depend on transport.
type Summarizer interface {
	Complete(ctx context.Context, req *oai.ChatCompletionRequest) (*oai.ChatCompletionResponse, error)
}

// Options configures a Compactor.
type Options struct {
	Estimator  *Estimator
	Summarizer Summarizer
	Log        *slog.Logger

	// Window is the model's context size in tokens. Zero disables compaction
	// entirely, because without a window there is nothing to compare against
	// and guessing would be worse than doing nothing.
	Window int

	Threshold float64
	Retain    float64
}

// Compactor keeps a conversation inside the context window.
type Compactor struct {
	est        *Estimator
	summarizer Summarizer
	log        *slog.Logger
	window     int
	threshold  float64
	retain     float64
}

// New builds a Compactor.
func New(opts Options) *Compactor {
	c := &Compactor{
		est:        opts.Estimator,
		summarizer: opts.Summarizer,
		log:        opts.Log,
		window:     opts.Window,
		threshold:  opts.Threshold,
		retain:     opts.Retain,
	}
	if c.est == nil {
		c.est = NewEstimator()
	}
	if c.threshold <= 0 || c.threshold >= 1 {
		c.threshold = DefaultThreshold
	}
	if c.retain <= 0 || c.retain >= 1 {
		c.retain = DefaultRetain
	}
	return c
}

// Method names how a compaction was achieved.
type Method string

const (
	// MethodNone means nothing was needed.
	MethodNone Method = ""
	// MethodPrune means old tool-result bodies were dropped.
	MethodPrune Method = "prune"
	// MethodSummarize means older turns were replaced by a summary.
	MethodSummarize Method = "summarize"
)

// Result describes what a compaction did.
type Result struct {
	Method   Method
	Messages []oai.Message

	BeforeTokens int
	AfterTokens  int

	// PrunedPaths are the files whose contents were dropped from the
	// conversation. Their observations must be invalidated: the model no
	// longer holds those bytes, so the read-before-edit check would otherwise
	// be vouching for contents nobody can see.
	PrunedPaths []string

	// DroppedAll reports that a summary replaced messages wholesale, so every
	// observation older than now is suspect.
	DroppedAll bool
}

// Compacted reports whether anything changed.
func (r *Result) Compacted() bool { return r != nil && r.Method != MethodNone }

// Estimator exposes the estimator so callers can feed it real usage reports.
func (c *Compactor) Estimator() *Estimator { return c.est }

// Window is the configured context size, zero if unknown.
func (c *Compactor) Window() int { return c.window }

// Maybe compacts the conversation if it is close to the window, and otherwise
// returns it unchanged.
func (c *Compactor) Maybe(
	ctx context.Context, msgs []oai.Message, tools []oai.Tool,
) (*Result, error) {
	if c.window <= 0 || len(msgs) < 4 {
		return &Result{Method: MethodNone, Messages: msgs}, nil
	}

	before := c.est.Estimate(msgs, tools)
	limit := int(float64(c.window) * c.threshold)
	if before < limit {
		return &Result{Method: MethodNone, Messages: msgs}, nil
	}

	return c.Force(ctx, msgs, tools, before)
}

// Force compacts regardless of the current size. Used for overflow recovery,
// where the provider has already told us the estimate was wrong.
func (c *Compactor) Force(
	ctx context.Context, msgs []oai.Message, tools []oai.Tool, before int,
) (*Result, error) {
	if before == 0 {
		before = c.est.Estimate(msgs, tools)
	}
	retainFrom := c.retainIndex(msgs)

	// First: drop the bodies of old tool results. Free, and usually enough.
	pruned, paths := pruneToolResults(msgs, retainFrom)
	after := c.est.Estimate(pruned, tools)
	limit := int(float64(c.window) * c.threshold)

	if after < limit {
		c.log.Info("context pruned",
			"before_tokens", before, "after_tokens", after,
			"window", c.window, "paths", len(paths))
		return &Result{
			Method: MethodPrune, Messages: pruned,
			BeforeTokens: before, AfterTokens: after, PrunedPaths: paths,
		}, nil
	}

	// Second: summarise everything before the retained tail.
	summarized, err := c.summarize(ctx, pruned, retainFrom)
	if err != nil {
		// A failed summary is not a failed request. The pruned conversation is
		// still smaller than what we started with, and sending it is more
		// likely to work than sending the original.
		c.log.Warn("summarisation failed; continuing with the pruned conversation",
			"error", err)
		return &Result{
			Method: MethodPrune, Messages: pruned,
			BeforeTokens: before, AfterTokens: after, PrunedPaths: paths,
		}, nil
	}

	final := c.est.Estimate(summarized, tools)
	c.log.Info("context summarised",
		"before_tokens", before, "after_tokens", final, "window", c.window)

	return &Result{
		Method: MethodSummarize, Messages: summarized,
		BeforeTokens: before, AfterTokens: final,
		PrunedPaths: paths, DroppedAll: true,
	}, nil
}

// retainIndex is the first message index kept verbatim.
//
// The index is then moved forward to a safe boundary. Cutting between an
// assistant message carrying tool_calls and the tool results answering them
// leaves an unanswered call in the history, and strict providers reject the
// whole request rather than the offending message — which surfaces as a 400
// on the turn *after* compaction, with nothing in it pointing at compaction.
func (c *Compactor) retainIndex(msgs []oai.Message) int {
	budget := int(float64(c.window) * c.retain)

	total := 0
	idx := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		total += c.est.EstimateMessages(msgs[i : i+1])
		if total > budget {
			break
		}
		idx = i
	}

	// Never summarise away the whole conversation, and never touch the
	// system prompt or the first user message: the task statement is the one
	// thing that must survive intact.
	if idx > len(msgs)-2 {
		idx = len(msgs) - 2
	}
	if idx < 2 {
		idx = 2
	}
	return safeCut(msgs, idx)
}

// safeCut moves an index forward until cutting there leaves no tool call
// unanswered.
func safeCut(msgs []oai.Message, idx int) int {
	for i := idx; i < len(msgs); i++ {
		if msgs[i].Role != oai.RoleTool {
			return i
		}
	}
	return len(msgs)
}

// pruneToolResults replaces the bodies of large tool results before retainFrom
// with a stub, and reports which file paths lost their contents.
func pruneToolResults(msgs []oai.Message, retainFrom int) ([]oai.Message, []string) {
	out := make([]oai.Message, len(msgs))
	copy(out, msgs)

	// Tool call id to the path it read, so a pruned result can name the file
	// whose observation is no longer backed by anything in context.
	readPaths := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if path := argPath(tc.Function.Arguments); path != "" {
				readPaths[tc.ID] = path
			}
		}
	}

	var paths []string
	for i := 0; i < retainFrom && i < len(out); i++ {
		m := out[i]
		if m.Role != oai.RoleTool {
			continue
		}
		body := m.Content.String()
		if len(body) <= prunedResultBytes {
			continue
		}

		label := "output"
		if p, ok := readPaths[m.ToolCallID]; ok {
			label = p
			paths = append(paths, p)
		}
		m.Content = oai.TextContent(fmt.Sprintf(
			"[%s omitted to save context — %d bytes. Re-read it if you still need it.]",
			label, len(body)))
		out[i] = m
	}
	return out, paths
}

// argPath pulls a "path" argument out of a tool call, if there is one.
func argPath(args string) string {
	var parsed struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return ""
	}
	return parsed.Path
}

// summarizePrompt is deliberately specific about what to keep.
//
// A generic "summarise this" produces a readable paragraph that is useless to
// an agent: it describes the conversation instead of preserving the state the
// model needs to continue. What matters is what was changed and where, what
// was learned about the code, and what is still outstanding — facts, not
// narrative.
const summarizePrompt = `Summarise the work so far so that it can be continued after the earlier messages are discarded.

Write it as notes, not prose. Include, in this order:

1. The task, in one or two sentences.
2. Every file created, modified or deleted, with its path and what changed.
3. Facts discovered about the codebase that were not obvious and would cost tool calls to rediscover: where things live, how something is structured, what a command output.
4. What has been tried and did not work, so it is not tried again.
5. What still needs doing.

Be specific. "Updated the config" is useless; "added shell.enabled to internal/config/config.go, defaulting true" is what is needed. Do not include file contents. Do not speculate about anything not established above.`

// summarize replaces everything before retainFrom with one summary message.
func (c *Compactor) summarize(
	ctx context.Context, msgs []oai.Message, retainFrom int,
) ([]oai.Message, error) {
	if c.summarizer == nil {
		return nil, fmt.Errorf("no summarizer configured")
	}
	if retainFrom <= 1 || retainFrom > len(msgs) {
		return nil, fmt.Errorf("nothing safe to summarise")
	}

	// Two things survive verbatim, ahead of the summary.
	//
	// The system prompt, because it is instructions rather than history.
	//
	// And the first user message, because it is the task. Letting the
	// summariser carry it is fragile in the worst way: if it paraphrases
	// loosely or drops the goal, the agent continues working with no idea
	// what it was asked to do, and the run fails in a way that looks like
	// model incompetence rather than context loss.
	var head []oai.Message
	start := 0
	if msgs[0].Role == oai.RoleSystem {
		head = append(head, msgs[0])
		start = 1
	}
	if start < len(msgs) && msgs[start].Role == oai.RoleUser {
		head = append(head, msgs[start])
		start++
	}

	if start >= retainFrom {
		return nil, fmt.Errorf("nothing to summarise")
	}
	toSummarize := msgs[start:retainFrom]
	if len(toSummarize) == 0 {
		return nil, fmt.Errorf("nothing to summarise")
	}

	// The transcript goes in as a single user message rather than as the
	// conversation itself: asking a model to summarise a history it is being
	// handed as its own history produces a continuation, not a summary.
	req := &oai.ChatCompletionRequest{
		Messages: []oai.Message{
			{Role: oai.RoleSystem, Content: oai.TextContent(summarizePrompt)},
			{Role: oai.RoleUser, Content: oai.TextContent(renderTranscript(toSummarize))},
		},
	}

	resp, err := c.summarizer.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("summariser returned no choices")
	}
	summary := strings.TrimSpace(resp.Choices[0].Message.Content.String())
	if summary == "" {
		return nil, fmt.Errorf("summariser returned nothing")
	}

	out := make([]oai.Message, 0, len(msgs)-retainFrom+len(head)+1)
	out = append(out, head...)
	out = append(out, oai.Message{
		Role: oai.RoleUser,
		Content: oai.TextContent(
			"[Earlier messages were removed to save context. Summary of the work so far:]\n\n" +
				summary),
	})
	out = append(out, msgs[retainFrom:]...)
	return out, nil
}

// renderTranscript flattens messages into text for the summariser.
func renderTranscript(msgs []oai.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "## %s\n", m.Role)
		if text := m.Content.String(); text != "" {
			b.WriteString(text)
			b.WriteString("\n")
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "[called %s with %s]\n", tc.Function.Name, tc.Function.Arguments)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// overflowPattern recognises a provider saying the context was exceeded.
//
// Matched rather than inferred from a status code because the status varies:
// some providers return 400, some 413, and at least one returns 200 with the
// error inside the stream.
var overflowPattern = regexp.MustCompile(`(?i)` +
	`context[_ ]length[_ ]exceeded` +
	`|maximum context length` +
	`|context window` +
	`|too many tokens` +
	`|reduce the length of the messages` +
	`|input is too long` +
	`|prompt is too long`)

// IsOverflow reports whether an error means the conversation was too long.
func IsOverflow(err error) bool {
	return err != nil && overflowPattern.MatchString(err.Error())
}
