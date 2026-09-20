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

func TestPromptStatesTheRulesTheLedgerEnforces(t *testing.T) {
	// Measured against nvidia/nemotron-3.5-lightning: creating a file took four
	// turns, because the prompt said "read before editing" and the ledger also
	// requires it before creating. The model went straight to write, was
	// refused with FS_NOT_OBSERVED, then read the missing path and succeeded.
	// It recovered correctly — the error message was fine. The prompt was the
	// defect.
	guidance := toolGuidance

	if !strings.Contains(guidance, "before editing it") {
		t.Error("the prompt does not state the read-before-edit rule")
	}

	lower := strings.ToLower(guidance)
	if !strings.Contains(lower, "creating a new file") {
		t.Error("the prompt does not state that creation needs a prior read; " +
			"the model will discover it by being refused, one wasted turn per file")
	}

	// The non-obvious half: a read that reports nothing there is not a failure,
	// it is the permission. Without saying so, "read it first" reads as absurd
	// advice for a file that does not exist yet.
	if !strings.Contains(lower, "nothing is there") &&
		!strings.Contains(lower, "reporting that nothing") {
		t.Error("the prompt does not explain that the failing read is what permits " +
			"the write, which is the part a model cannot infer")
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
