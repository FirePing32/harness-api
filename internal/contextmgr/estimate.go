// Package contextmgr keeps a conversation inside the model's context window.
package contextmgr

import (
	"sync"

	"github.com/FirePing32/harness-api/internal/oai"
)

// Estimating token counts without a tokenizer.
//
// The obvious approach is to vendor tiktoken. It is also wrong here: this
// server talks to whatever endpoint it is pointed at, and OpenAI's tokenizer
// says nothing useful about Llama, Qwen or DeepSeek. A count that is confidently
// wrong is worse than one that knows it is approximate, because compaction
// decisions get made on it.
//
// So the estimate starts as a byte ratio and then corrects itself. Every
// upstream response carries usage.prompt_tokens for a request whose byte count
// we know exactly, which is a free labelled sample. Feeding those into an
// exponentially-weighted average converges on the real tokenizer's behaviour
// within a handful of turns, for any tokenizer, with no dependency.
//
// What matters is not precision but which side the error falls on. Compaction
// triggers at a fraction of the window, so underestimating means overflowing
// and overestimating means compacting early. Overflow is a failed request;
// early compaction costs one summarisation. The ratio is therefore nudged
// toward caution when it is still unsure.

const (
	// initialRatio is bytes per token before any correction. Code and JSON sit
	// near 3, prose near 4, and an agent conversation is mostly the former
	// wrapped in the latter.
	initialRatio = 3.4

	// alpha is the EWMA weight for each new observation. High enough to
	// converge in a few turns, low enough that one unusual message does not
	// swing it.
	alpha = 0.3

	// warmupSamples is how many observations are needed before the estimate is
	// trusted at face value.
	warmupSamples = 3

	// warmupMargin inflates the estimate while still warming up. Compacting
	// slightly early is cheap; overflowing is a failed request.
	warmupMargin = 1.15

	// ratio bounds. A pathological message — a single base64 blob, or a file
	// of CJK text — can produce a sample far outside anything real, and
	// letting it into the average would poison every later decision.
	minRatio = 1.5
	maxRatio = 8.0
)

// Estimator converts message bytes into an approximate token count and
// corrects itself against real usage reports.
// Safe for concurrent use.
type Estimator struct {
	mu      sync.Mutex
	ratio   float64
	samples int
}

// NewEstimator returns an estimator that has not yet seen a real count.
func NewEstimator() *Estimator {
	return &Estimator{ratio: initialRatio}
}

// Estimate approximates the prompt tokens for a request.
func (e *Estimator) Estimate(msgs []oai.Message, tools []oai.Tool) int {
	return e.tokensFor(Bytes(msgs) + toolBytes(tools))
}

// EstimateMessages approximates the tokens for a message slice alone.
func (e *Estimator) EstimateMessages(msgs []oai.Message) int {
	return e.tokensFor(Bytes(msgs))
}

func (e *Estimator) tokensFor(bytes int) int {
	e.mu.Lock()
	ratio, samples := e.ratio, e.samples
	e.mu.Unlock()

	est := float64(bytes) / ratio
	if samples < warmupSamples {
		est *= warmupMargin
	}
	return int(est)
}

// Observe records a real prompt-token count for a request of known size.
//
// Samples outside the plausible range are ignored rather than clamped: a
// wildly wrong sample usually means the byte count and the token count
// describe different things — a cached prompt, a provider counting images —
// and averaging that in would corrupt every later decision.
func (e *Estimator) Observe(bytes, promptTokens int) {
	if bytes <= 0 || promptTokens <= 0 {
		return
	}
	sample := float64(bytes) / float64(promptTokens)
	if sample < minRatio || sample > maxRatio {
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.ratio = (1-alpha)*e.ratio + alpha*sample
	e.samples++
}

// Ratio is the current bytes-per-token estimate.
func (e *Estimator) Ratio() float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ratio
}

// Samples is how many real usage reports have been folded in.
func (e *Estimator) Samples() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.samples
}

// perMessageOverhead approximates the role markers and delimiters a chat
// template adds around every message. Small, but an agent conversation has
// hundreds of short tool results and the overhead stops being negligible.
const perMessageOverhead = 16

// Bytes is the serialised size of a message slice, close enough for the ratio
// to correct the rest.
func Bytes(msgs []oai.Message) int {
	total := 0
	for _, m := range msgs {
		total += perMessageOverhead + len(m.Role) + len(m.Content.String())
		total += len(m.Name) + len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			total += len(tc.ID) + len(tc.Function.Name) + len(tc.Function.Arguments) + 8
		}
		// Reasoning is counted because the provider bills for producing it,
		// even though it is stripped before the next request goes out.
		total += len(m.ReasoningContent)
	}
	return total
}

// toolBytes is the size of the tool definitions, which are re-sent on every
// single turn and are far from free — six tools with prose descriptions is a
// few thousand tokens of fixed overhead per call.
func toolBytes(tools []oai.Tool) int {
	total := 0
	for _, t := range tools {
		total += len(t.Function.Name) + len(t.Function.Description) +
			len(t.Function.Parameters) + perMessageOverhead
	}
	return total
}
