// Package agent runs the model-tool-model loop behind one API request.
package agent

import (
	"fmt"
	"sync"
	"time"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/oai"
)

// A runaway agent loop is a billing incident before it is anything else.
//
// The failure is not dramatic: the model calls a tool, misreads the result,
// calls it again, and does that until something stops it. Nothing throws. Every
// individual request looks fine. What gives it away is only visible in
// aggregate, which is why every dimension here has a finite default and none
// of them can be raised by a request — a caller may lower a ceiling but never
// lift it, because the caller is not the one paying the upstream bill.

// StopReason says why the loop ended. It maps onto the finish_reason the client
// sees, but carries more detail than the four OpenAI values allow.
type StopReason string

const (
	// StopComplete is the model answering without asking for another tool.
	StopComplete StopReason = "complete"
	// StopMaxIterations means the loop hit its turn ceiling.
	StopMaxIterations StopReason = "max_iterations"
	// StopWallClock means the request ran out of time.
	StopWallClock StopReason = "wall_clock"
	// StopTokens means the upstream token ceiling was reached.
	StopTokens StopReason = "max_tokens"
	// StopCancelled means the client went away or the context was cancelled.
	StopCancelled StopReason = "cancelled"
	// StopError means an unrecoverable failure, not a ceiling.
	StopError StopReason = "error"
)

// FinishReason maps a stop reason onto the OpenAI vocabulary.
//
// Every ceiling reports "length". It is not a perfect fit for an iteration cap,
// but it is the only standard value that means "stopped early, the answer is
// incomplete", and a client that handles truncation will do the right thing
// with it. Inventing a value here would simply be ignored.
func (r StopReason) FinishReason() string {
	switch r {
	case StopComplete:
		return oai.FinishStop
	case StopCancelled:
		return oai.FinishStop
	default:
		return oai.FinishLength
	}
}

// Terminal reports whether the loop must stop.
func (r StopReason) Terminal() bool { return r != "" && r != StopComplete }

// Budget is the set of ceilings applied to one request.
type Budget struct {
	MaxIterations  int
	MaxWallClock   time.Duration
	MaxTotalTokens int
}

// BudgetFrom builds a Budget from configuration.
func BudgetFrom(cfg config.Agent) Budget {
	return Budget{
		MaxIterations:  cfg.MaxIterations,
		MaxWallClock:   cfg.MaxWallClock.Duration(),
		MaxTotalTokens: cfg.MaxTotalTokens,
	}
}

// WithRequestOverride applies a caller's requested iteration cap, downward only.
func (b Budget) WithRequestOverride(maxIterations *int) Budget {
	if maxIterations != nil && *maxIterations > 0 && *maxIterations < b.MaxIterations {
		b.MaxIterations = *maxIterations
	}
	return b
}

// Tracker accounts for one request against its budget.
// Safe for concurrent use; usage is added from tool goroutines.
type Tracker struct {
	budget Budget
	start  time.Time

	mu         sync.Mutex
	iterations int
	usage      oai.Usage
}

// NewTracker starts accounting now.
func NewTracker(b Budget) *Tracker {
	return &Tracker{budget: b, start: time.Now()}
}

// BeginIteration accounts for one more model call, or reports why it cannot.
//
// Checked before the call rather than after, because the expensive thing is the
// request we are about to make, not the one we just finished.
func (t *Tracker) BeginIteration() StopReason {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.budget.MaxIterations > 0 && t.iterations >= t.budget.MaxIterations {
		return StopMaxIterations
	}
	if t.budget.MaxWallClock > 0 && time.Since(t.start) >= t.budget.MaxWallClock {
		return StopWallClock
	}
	if t.budget.MaxTotalTokens > 0 && t.usage.TotalTokens >= t.budget.MaxTotalTokens {
		return StopTokens
	}

	t.iterations++
	return ""
}

// AddUsage records what an upstream call cost.
func (t *Tracker) AddUsage(u *oai.Usage) {
	if u == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.usage.Add(u)
}

// Usage is the total across every upstream call in this request.
//
// Reporting the aggregate rather than the last call's numbers is the honest
// choice: the client asked one question and this is what answering it cost.
// Returning only the final turn's usage would understate a twenty-tool-call
// request by more than an order of magnitude.
func (t *Tracker) Usage() oai.Usage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.usage
}

// Iterations is how many model calls have been started.
func (t *Tracker) Iterations() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.iterations
}

// Elapsed is how long the request has been running.
func (t *Tracker) Elapsed() time.Duration { return time.Since(t.start) }

// Remaining reports the wall clock left, or zero if there is no limit.
func (t *Tracker) Remaining() time.Duration {
	if t.budget.MaxWallClock <= 0 {
		return 0
	}
	return max(t.budget.MaxWallClock-t.Elapsed(), 0)
}

// ExhaustionNote is the text appended to a truncated answer.
//
// A loop that stops at a ceiling and returns whatever happened to be in the
// last message is indistinguishable, to the client, from a model that finished.
// Saying so in the content costs a sentence and prevents a partial result being
// read as a complete one.
func (t *Tracker) ExhaustionNote(r StopReason) string {
	switch r {
	case StopMaxIterations:
		return fmt.Sprintf(
			"\n\n[Stopped after %d tool-calling turns, the limit for one request. "+
				"The task may be unfinished; ask again to continue from here.]",
			t.Iterations())
	case StopWallClock:
		return fmt.Sprintf(
			"\n\n[Stopped after %s, the time limit for one request. "+
				"The task may be unfinished; ask again to continue from here.]",
			t.Elapsed().Round(time.Second))
	case StopTokens:
		return fmt.Sprintf(
			"\n\n[Stopped after %d tokens, the limit for one request. "+
				"The task may be unfinished; ask again to continue from here.]",
			t.Usage().TotalTokens)
	default:
		return ""
	}
}
