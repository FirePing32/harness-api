package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Progress events.
//
// A request that runs for two minutes over fifteen tool calls looks identical,
// from the outside, to one that has hung. These events exist so a client that
// wants to can watch the work happen. They are opt-in: standard OpenAI clients
// neither ask for them nor would know what to do with them.
//
// The events carry the structured form of what happened, not prose, so a UI can
// render a tool call however it likes. Summary exists only for clients that
// want a line of text without interpreting the rest.

// EventKind identifies what happened.
type EventKind string

const (
	// EventTurnStart is emitted before each upstream call.
	EventTurnStart EventKind = "turn_start"
	// EventToolStart is emitted before a tool runs.
	EventToolStart EventKind = "tool_start"
	// EventToolEnd is emitted when a tool finishes, successfully or not.
	EventToolEnd EventKind = "tool_end"
	// EventDone is emitted once, when the loop stops.
	EventDone EventKind = "done"
)

// Event is one thing that happened during an agent run.
type Event struct {
	Type EventKind `json:"type"`
	Turn int       `json:"turn,omitempty"`

	Tool   string `json:"tool,omitempty"`
	CallID string `json:"call_id,omitempty"`

	// Args is the tool's raw argument JSON, exactly as the model produced it.
	Args json.RawMessage `json:"args,omitempty"`

	IsError bool   `json:"is_error,omitempty"`
	Code    string `json:"code,omitempty"`

	DurationMS int64 `json:"duration_ms,omitempty"`

	// Stop and Usage are set on the done event.
	Stop        StopReason `json:"stop,omitempty"`
	TotalTokens int        `json:"total_tokens,omitempty"`

	// Summary is a short line of text for clients that do not want to
	// interpret the structured fields.
	Summary string `json:"summary,omitempty"`
}

// Emit is how the loop reports progress. A nil Emit is valid and costs nothing,
// which is the common case.
type Emit func(Event)

func (e Emit) emit(ev Event) {
	if e != nil {
		e(ev)
	}
}

// summariseCall renders a tool call as one short line.
//
// It reaches into the arguments for the one field that identifies what is being
// worked on — a path, a pattern, a command — because "read" on its own tells a
// watcher nothing, and the full argument JSON is too much for a status line.
func summariseCall(tool string, args json.RawMessage) string {
	var parsed map[string]any
	if err := json.Unmarshal(args, &parsed); err != nil {
		return tool
	}

	for _, key := range []string{"command", "path", "pattern"} {
		v, ok := parsed[key].(string)
		if !ok || v == "" {
			continue
		}
		v = strings.TrimSpace(strings.ReplaceAll(v, "\n", " "))
		if len(v) > 80 {
			v = v[:77] + "..."
		}
		return fmt.Sprintf("%s %s", tool, v)
	}
	return tool
}
