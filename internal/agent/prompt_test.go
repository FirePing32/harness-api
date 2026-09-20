package agent

import (
	"strings"
	"testing"
)

// Every rule the workspace enforces has to be stated here.
//
// A rule that is enforced but unstated does not stop the model doing the wrong
// thing; it makes the model discover the rule by being refused, which costs a
// turn and the tokens of a whole extra round trip. That is not hypothetical —
// it is what the first real run against a model measured, and these assertions
// exist so a future trim of the prompt cannot quietly put it back.

func TestPromptStatesTheRuleTheLedgerEnforces(t *testing.T) {
	// Read-before-edit is enforced, so it has to be stated: a rule the model
	// can only discover by being refused costs a turn every time.
	//
	// Creation deliberately has no such rule any more. An earlier version
	// required a confirmed absence before creating a file, and stating that
	// here did not stop the model going straight to write — measured, twice.
	// The rule was then removed as not earning its cost, so there is nothing
	// left to say about it.
	if !strings.Contains(toolGuidance, "before editing it") {
		t.Error("the prompt does not state the read-before-edit rule")
	}
}

func TestPromptTellsTheModelWhatToDoWithAFailedEdit(t *testing.T) {
	// Tool errors are the model's only recovery signal, and the prompt has to
	// point at them or they are just noise before a retry of the same call.
	if !strings.Contains(toolGuidance, "read the error") {
		t.Error("the prompt does not tell the model to read a failed edit's error")
	}
}

func TestPromptSaysRefusingIsAllowed(t *testing.T) {
	// Without this, an impossible request gets a plausible substitute rather
	// than a refusal, which is worse than doing nothing.
	if !strings.Contains(toolGuidance, "cannot be done") {
		t.Error("the prompt does not license refusing an impossible task")
	}
}
