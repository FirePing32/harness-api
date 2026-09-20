package contextmgr

import (
	"math"
	"strings"
	"testing"

	"github.com/FirePing32/harness-api/internal/oai"
)

func TestEstimatorConvergesOnTheRealTokenizer(t *testing.T) {
	// The point of the whole design: no vendored tokenizer, but after a few
	// real usage reports the estimate tracks whatever tokenizer this provider
	// actually uses.
	e := NewEstimator()

	const realRatio = 2.6 // a tokenizer denser than the default guess
	bytes := 10_000
	for range 12 {
		e.Observe(bytes, int(float64(bytes)/realRatio))
	}

	if got := e.Ratio(); math.Abs(got-realRatio) > 0.15 {
		t.Errorf("ratio = %.2f after 12 samples, want it near %.2f", got, realRatio)
	}
}

func TestEstimatorIsCautiousUntilItHasEvidence(t *testing.T) {
	// Underestimating means overflowing, which is a failed request.
	// Overestimating means compacting early, which costs one summarisation.
	// The error should fall on the cheap side while still unsure.
	cold := NewEstimator()
	warm := NewEstimator()
	for range warmupSamples {
		warm.Observe(3400, 1000) // exactly the initial ratio, so only the margin differs
	}

	msgs := []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent(strings.Repeat("x", 3400))}}
	if cold.EstimateMessages(msgs) <= warm.EstimateMessages(msgs) {
		t.Error("a cold estimator should read high, not low")
	}
}

func TestEstimatorRejectsImplausibleSamples(t *testing.T) {
	// A sample far outside anything real usually means the byte count and the
	// token count describe different things — a cached prompt, a provider
	// counting images — and averaging it in would corrupt every later decision.
	e := NewEstimator()
	before := e.Ratio()

	e.Observe(1_000_000, 1) // 1e6 bytes per token
	e.Observe(10, 100)      // 0.1 bytes per token
	e.Observe(0, 100)       // no bytes
	e.Observe(100, 0)       // no tokens, as when a provider omits usage

	if e.Ratio() != before {
		t.Errorf("ratio moved to %.2f on implausible samples", e.Ratio())
	}
	if e.Samples() != 0 {
		t.Errorf("Samples = %d, want implausible samples ignored", e.Samples())
	}
}

func TestEstimatorStaysWithinBoundsUnderPressure(t *testing.T) {
	e := NewEstimator()
	for range 50 {
		e.Observe(1000, 600) // ~1.67, just above the floor
	}
	if r := e.Ratio(); r < minRatio || r > maxRatio {
		t.Errorf("ratio = %.2f, outside the plausible band", r)
	}
}

func TestBytesCountsEverythingThatIsSent(t *testing.T) {
	// Tool call arguments and reasoning are billed too. Counting only content
	// would understate an agent conversation, where most of the volume is tool
	// traffic.
	msgs := []oai.Message{{
		Role:    oai.RoleAssistant,
		Content: oai.TextContent("checking"),
		ToolCalls: []oai.ToolCall{{
			ID: "c1", Function: oai.FunctionCall{
				Name: "read", Arguments: `{"path":"a-very-long-file-name.go"}`,
			},
		}},
		ReasoningContent: "the user wants the file",
	}}

	got := Bytes(msgs)
	minimum := len("checking") + len(`{"path":"a-very-long-file-name.go"}`) +
		len("the user wants the file")
	if got <= minimum {
		t.Errorf("Bytes = %d, want more than the %d bytes of raw payload", got, minimum)
	}
}

func TestBytesGrowsWithTheConversation(t *testing.T) {
	short := []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}}
	long := []oai.Message{
		{Role: oai.RoleUser, Content: oai.TextContent(strings.Repeat("x", 5000))},
	}
	if Bytes(long) <= Bytes(short) {
		t.Error("Bytes does not grow with content")
	}
}

func TestEstimateIncludesToolDefinitions(t *testing.T) {
	// Tool definitions are re-sent on every turn. Six tools with prose
	// descriptions is a few thousand tokens of fixed overhead per call, and
	// ignoring it means compacting too late.
	e := NewEstimator()
	msgs := []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent("hi")}}
	tools := []oai.Tool{{Function: oai.FunctionDef{
		Name:        "read",
		Description: strings.Repeat("a long description ", 100),
		Parameters:  []byte(`{"type":"object"}`),
	}}}

	if e.Estimate(msgs, tools) <= e.Estimate(msgs, nil) {
		t.Error("tool definitions are not counted")
	}
}

func TestEstimatorIsSafeUnderConcurrency(t *testing.T) {
	// Observations arrive from request goroutines while estimates are read.
	e := NewEstimator()
	done := make(chan struct{})
	for i := range 8 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := range 100 {
				e.Observe(1000+j, 300+i)
				e.Ratio()
				e.EstimateMessages([]oai.Message{
					{Role: oai.RoleUser, Content: oai.TextContent("x")},
				})
			}
		}(i)
	}
	for range 8 {
		<-done
	}
}
