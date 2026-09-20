package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/FirePing32/harness-api/internal/audit"
	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/contextmgr"
	"github.com/FirePing32/harness-api/internal/guard"
	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/tools"
	"github.com/FirePing32/harness-api/internal/upstream"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// Loop turns one client request into as many upstream calls as the task needs.
type Loop struct {
	upstream  *upstream.Client
	registry  *tools.Registry
	guards    *guard.Chain
	compactor *contextmgr.Compactor
	cfg       config.Agent
	log       *slog.Logger
	audit     *audit.Logger

	// dialect is how much JSON Schema the configured provider tolerates.
	dialect tools.SchemaDialect
}

// Options configures a Loop.
type Options struct {
	Upstream  *upstream.Client
	Registry  *tools.Registry
	Guards    *guard.Chain
	Compactor *contextmgr.Compactor
	Config    config.Agent
	Log       *slog.Logger
	Audit     *audit.Logger
	Dialect   tools.SchemaDialect
}

// New builds a Loop.
func New(opts Options) *Loop {
	dialect := opts.Dialect
	if dialect == "" {
		dialect = tools.DialectFull
	}
	return &Loop{
		upstream:  opts.Upstream,
		registry:  opts.Registry,
		guards:    opts.Guards,
		compactor: opts.Compactor,
		cfg:       opts.Config,
		log:       opts.Log,
		audit:     opts.Audit,
		dialect:   dialect,
	}
}

// Result is a finished agent run.
type Result struct {
	// Messages is everything the loop added: assistant turns and tool results,
	// in order. The final assistant message is the answer.
	Messages []oai.Message
	Final    oai.Message
	Stop     StopReason
	Usage    oai.Usage
	Turns    int
	Elapsed  time.Duration
}

// Run drives the loop to completion.
//
// The session must already be locked by the caller and stays locked for the
// whole run: tools mutate the workspace and the observation ledger, and a
// second request interleaving with this one would produce failures that are
// impossible to reconstruct from a transcript.
func (l *Loop) Run(ctx context.Context, s *workspace.Session, req *oai.ChatCompletionRequest) (*Result, error) {
	return l.RunWithEvents(ctx, s, req, nil)
}

// RunWithEvents drives the loop, reporting progress as it goes. A nil emit
// behaves exactly like Run.
func (l *Loop) RunWithEvents(ctx context.Context, s *workspace.Session, req *oai.ChatCompletionRequest, emit Emit) (*Result, error) {
	budget := BudgetFrom(l.cfg)
	if req.Harness != nil {
		budget = budget.WithRequestOverride(req.Harness.MaxIterations)
	}
	tracker := NewTracker(budget)

	registry := l.registry
	if req.Harness != nil && len(req.Harness.Tools) > 0 {
		sub, err := registry.Subset(req.Harness.Tools)
		if err != nil {
			return nil, oai.NewInvalidRequest(err.Error(), "harness.tools")
		}
		registry = sub
	}

	callerSystem, conversation := SplitSystem(req.Messages)
	if len(conversation) == 0 {
		return nil, oai.NewInvalidRequest(
			"messages must contain at least one non-system message", "messages")
	}

	// The working history: our system prompt, then the client's conversation.
	// Rebuilt here rather than mutating req.Messages, which the caller still owns.
	history := make([]oai.Message, 0, len(conversation)+16)
	history = append(history, oai.Message{
		Role:    oai.RoleSystem,
		Content: oai.TextContent(Build(s, registry.Names(), callerSystem)),
	})
	history = append(history, conversation...)

	definitions := registry.Definitions(l.dialect)
	result := &Result{}

	// Every tool call made in this request, oldest first. Guards need it:
	// a single call in isolation never looks wrong, and the loops worth
	// catching are only visible as a pattern across several.
	var priorCalls []guard.Call

	// Monotonic, and deliberately not len(history). Compaction shortens the
	// history, so a length-derived turn number would go backwards and the
	// ledger's "invalidate everything observed before now" would silently
	// invalidate the wrong set.
	turnCounter := 0

	for {
		if stop := tracker.BeginIteration(); stop.Terminal() {
			return l.done(result, tracker, stop, emit), nil
		}
		if ctx.Err() != nil {
			return l.done(result, tracker, StopCancelled, emit), nil
		}

		emit.emit(Event{Type: EventTurnStart, Turn: result.Turns + 1})

		turnCounter++
		// The turn index stamps filesystem observations, so compaction can later
		// identify which ones it invalidated.
		s.SetTurn(turnCounter)

		history = l.compact(ctx, s, history, definitions, turnCounter, false)

		resp, err := l.completeWithOverflowRecovery(ctx, s, req, &history, definitions, turnCounter)
		if err != nil {
			if ctx.Err() != nil {
				return l.done(result, tracker, StopCancelled, emit), nil
			}
			return nil, err
		}
		tracker.AddUsage(resp.Usage)
		l.observeUsage(history, definitions, resp)

		if len(resp.Choices) == 0 {
			return nil, fmt.Errorf("upstream returned no choices")
		}
		assistant := resp.Choices[0].Message
		assistant.Role = oai.RoleAssistant

		history = append(history, assistant)
		result.Messages = append(result.Messages, assistant)
		result.Turns++

		if len(assistant.ToolCalls) == 0 {
			result.Final = assistant
			return l.done(result, tracker, StopComplete, emit), nil
		}

		l.log.Debug("tool calls requested",
			"session_id", s.ID(), "turn", result.Turns, "count", len(assistant.ToolCalls))

		results := l.runToolCalls(ctx, s, registry, assistant.ToolCalls,
			result.Turns, tracker, priorCalls, emit)
		for i, r := range results {
			msg := r.ToMessage()
			history = append(history, msg)
			result.Messages = append(result.Messages, msg)
			priorCalls = append(priorCalls, guard.Call{
				Tool:    assistant.ToolCalls[i].Function.Name,
				Args:    assistant.ToolCalls[i].Function.Arguments,
				IsError: r.IsError,
				Code:    r.Code,
			})
		}

		if ctx.Err() != nil {
			return l.done(result, tracker, StopCancelled, emit), nil
		}
	}
}

// done completes the Result and emits the closing event.
func (l *Loop) done(r *Result, t *Tracker, stop StopReason, emit Emit) *Result {
	out := l.finish(r, t, stop)
	emit.emit(Event{
		Type: EventDone, Turn: out.Turns, Stop: out.Stop,
		TotalTokens: out.Usage.TotalTokens,
	})
	return out
}

// completeWithOverflowRecovery sends a turn, and if the provider says the
// conversation was too long, compacts harder and sends it once more.
//
// The estimate is an approximation over an unknown tokenizer, so it will be
// wrong sometimes. When it is, the provider tells us — and an overflow is the
// one error where the right response is obvious and mechanical. Returning it
// to the caller would hand them a failure they can do nothing about, in the
// middle of a task that was going fine.
func (l *Loop) completeWithOverflowRecovery(
	ctx context.Context, s *workspace.Session, req *oai.ChatCompletionRequest,
	history *[]oai.Message, defs []oai.Tool, turn int,
) (*oai.ChatCompletionResponse, error) {
	resp, err := l.upstream.Complete(ctx, l.buildTurn(req, *history, defs))
	if err == nil || !contextmgr.IsOverflow(err) || l.compactor == nil {
		return resp, err
	}

	l.log.Warn("provider reported a context overflow; compacting and retrying",
		"session_id", s.ID(), "turn", turn)

	compacted := l.compact(ctx, s, *history, defs, turn, true)
	if len(compacted) == len(*history) {
		// Nothing could be freed, so retrying would fail identically.
		return nil, err
	}
	*history = compacted

	return l.upstream.Complete(ctx, l.buildTurn(req, compacted, defs))
}

// compact keeps the conversation inside the window and invalidates whatever
// filesystem observations it just invalidated the evidence for.
//
// That second part is the whole reason the ledger records a turn number. When
// a file's contents leave the conversation, the model no longer holds them,
// but the observation survives — and the read-before-edit check goes on
// vouching for bytes nobody can see. The invariant degrades into a rubber
// stamp, silently, and the failure it was built to prevent comes back.
func (l *Loop) compact(
	ctx context.Context, s *workspace.Session,
	history []oai.Message, defs []oai.Tool, turn int, force bool,
) []oai.Message {
	if l.compactor == nil {
		return history
	}

	var (
		res *contextmgr.Result
		err error
	)
	if force {
		res, err = l.compactor.Force(ctx, history, defs, 0)
	} else {
		res, err = l.compactor.Maybe(ctx, history, defs)
	}
	if err != nil || !res.Compacted() {
		if err != nil {
			l.log.Warn("compaction failed; continuing uncompacted",
				"session_id", s.ID(), "error", err)
		}
		return history
	}

	switch {
	case res.DroppedAll:
		// A summary replaced messages wholesale, so anything observed before
		// this turn is suspect. Conservative on purpose: the cost of being
		// wrong this way is one re-read, and the cost of the other way is an
		// edit applied to contents the model was only guessing at.
		n := s.Ledger().MarkStaleBefore(turn)
		l.log.Info("context compacted",
			"session_id", s.ID(), "method", res.Method,
			"before_tokens", res.BeforeTokens, "after_tokens", res.AfterTokens,
			"observations_invalidated", n)
	default:
		n := s.Ledger().MarkStale(res.PrunedPaths...)
		l.log.Info("context compacted",
			"session_id", s.ID(), "method", res.Method,
			"before_tokens", res.BeforeTokens, "after_tokens", res.AfterTokens,
			"observations_invalidated", n)
	}
	return res.Messages
}

// observeUsage feeds a real prompt-token count back into the estimator, which
// is how a byte ratio converges on whatever tokenizer this provider uses.
func (l *Loop) observeUsage(
	history []oai.Message, defs []oai.Tool, resp *oai.ChatCompletionResponse,
) {
	if l.compactor == nil || resp.Usage == nil || resp.Usage.PromptTokens == 0 {
		return
	}
	l.compactor.Estimator().Observe(
		contextmgr.Bytes(history)+toolBytes(defs), resp.Usage.PromptTokens)
}

func toolBytes(tools []oai.Tool) int {
	total := 0
	for _, t := range tools {
		total += len(t.Function.Name) + len(t.Function.Description) +
			len(t.Function.Parameters) + 16
	}
	return total
}

// buildTurn assembles one upstream request from the running history.
func (l *Loop) buildTurn(req *oai.ChatCompletionRequest, history []oai.Message, defs []oai.Tool) *oai.ChatCompletionRequest {
	turn := *req // shallow copy: sampling parameters carry through unchanged
	turn.Messages = history
	turn.Tools = defs
	turn.Stream = false
	turn.StreamOptions = nil
	turn.Harness = nil
	turn.N = nil
	return &turn
}

// finish completes a Result.
func (l *Loop) finish(r *Result, t *Tracker, stop StopReason) *Result {
	r.Stop = stop
	r.Usage = t.Usage()
	r.Elapsed = t.Elapsed()

	if r.Final.Role == "" {
		r.Final = oai.Message{Role: oai.RoleAssistant, Content: oai.TextContent("")}
	}

	// A run that stopped at a ceiling has to say so in the content. The client
	// sees finish_reason "length", but plenty of clients ignore it, and a
	// truncated answer that reads as complete is worse than a slow one.
	if note := t.ExhaustionNote(stop); note != "" {
		r.Final.Content = oai.TextContent(r.Final.Content.String() + note)
		if n := len(r.Messages); n > 0 {
			r.Messages[n-1] = r.Final
		} else {
			r.Messages = append(r.Messages, r.Final)
		}
	}
	return r
}

// runToolCalls executes a turn's tool calls and returns their results in the
// order the model asked for them.
//
// Ordering is not an aesthetic choice. Strict providers reject a request whose
// tool results do not correspond to the preceding tool_calls array, so the
// output order is fixed regardless of what finished first.
//
// Concurrency is opt-in per call. A tool that reads may run alongside other
// readers; anything that writes runs alone and acts as a barrier, so the calls
// around it observe a consistent filesystem. Treating "unsure" as exclusive
// means a new tool is safe by default, and the cost of being wrong in that
// direction is latency rather than corruption.
func (l *Loop) runToolCalls(
	ctx context.Context, s *workspace.Session, registry *tools.Registry,
	calls []oai.ToolCall, turn int, tracker *Tracker, history []guard.Call, emit Emit,
) []tools.Result {
	results := make([]tools.Result, len(calls))
	limit := max(l.cfg.MaxParallelTools, 1)

	for i := 0; i < len(calls); {
		if !l.concurrencySafe(registry, calls[i]) {
			results[i] = l.invoke(ctx, s, registry, calls[i], turn, tracker, history, emit)
			i++
			continue
		}

		// Gather the run of consecutive safe calls, capped at the parallel limit.
		end := i
		for end < len(calls) && end-i < limit && l.concurrencySafe(registry, calls[end]) {
			end++
		}

		if end-i == 1 {
			results[i] = l.invoke(ctx, s, registry, calls[i], turn, tracker, history, emit)
			i = end
			continue
		}

		var wg sync.WaitGroup
		for k := i; k < end; k++ {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				results[k] = l.invoke(ctx, s, registry, calls[k], turn, tracker, history, emit)
			}(k)
		}
		wg.Wait()
		i = end
	}

	return results
}

// concurrencySafe asks the tool whether this particular call may overlap others.
// An unknown tool is not safe: it will produce an error result, and doing that
// in call order keeps the transcript readable.
func (l *Loop) concurrencySafe(registry *tools.Registry, call oai.ToolCall) bool {
	t, ok := registry.Get(call.Function.Name)
	if !ok {
		return false
	}
	return t.ConcurrencySafe([]byte(call.Function.Arguments))
}

func (l *Loop) invoke(
	ctx context.Context, s *workspace.Session, registry *tools.Registry,
	call oai.ToolCall, turn int, tracker *Tracker, history []guard.Call, emit Emit,
) tools.Result {
	args := json.RawMessage(call.Function.Arguments)

	// Guards run before the tool, not inside it. A tool cannot see the rest of
	// the request, and the checks worth having - this is the fourth identical
	// call, there is not enough time left for this command - are only
	// answerable from outside.
	if reason, by := l.guards.Check(guard.Execution{
		Tool:      call.Function.Name,
		CallID:    call.ID,
		Args:      args,
		Turn:      turn,
		SessionID: s.ID(),
		Remaining: tracker.Remaining(),
		Prior:     history,
	}); reason != "" {
		l.log.Info("tool call denied",
			"session_id", s.ID(), "tool", call.Function.Name,
			"call_id", call.ID, "guard", by)
		// A refusal is the record most worth keeping: it is the one event that
		// says something tried to do what the guards exist to stop.
		l.audit.Denied(s.ID(), call.Function.Name, by, reason)
		emit.emit(Event{
			Type: EventToolEnd, Turn: turn,
			Tool: call.Function.Name, CallID: call.ID,
			IsError: true, Code: tools.CodeDenied,
			Summary: "denied by " + by,
		})
		return tools.Denied(call, reason)
	}
	emit.emit(Event{
		Type: EventToolStart, Turn: turn,
		Tool: call.Function.Name, CallID: call.ID, Args: args,
		Summary: summariseCall(call.Function.Name, args),
	})

	start := time.Now()
	res := registry.Invoke(ctx, s, call)
	elapsed := time.Since(start)

	emit.emit(Event{
		Type: EventToolEnd, Turn: turn,
		Tool: call.Function.Name, CallID: call.ID,
		IsError: res.IsError, Code: res.Code,
		DurationMS: elapsed.Milliseconds(),
	})

	level := slog.LevelDebug
	if res.IsError {
		// Tool errors are normal, but their rate per tool is the direct signal
		// about whether a tool's messages are instructive enough, so they are
		// logged at a level an operator will actually see.
		level = slog.LevelInfo
	}
	l.log.Log(ctx, level, "tool call",
		"session_id", s.ID(),
		"tool", call.Function.Name,
		"call_id", call.ID,
		"error", res.IsError,
		"code", res.Code,
		"duration_ms", elapsed.Milliseconds())

	return res
}

// ToResponse renders a finished run as a chat completion.
func (r *Result) ToResponse(id, model string, created int64) *oai.ChatCompletionResponse {
	finish := r.Stop.FinishReason()
	usage := r.Usage

	return &oai.ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []oai.Choice{{
			Index:        0,
			Message:      r.Final,
			FinishReason: &finish,
		}},
		Usage: &usage,
	}
}
