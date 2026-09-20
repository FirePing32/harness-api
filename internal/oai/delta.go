package oai

import (
	"sort"
	"strconv"
	"strings"
)

// Reassembling a streamed completion.
//
// Tool calls arrive fragmented across chunks, and the rules for putting them back
// together are where most OpenAI-compatible clients have bugs. The contract this
// implements:
//
//   - `index` identifies a call, not array position and not id. One chunk may
//     carry fragments for several indices, out of order and with gaps.
//   - `id` and `name` normally arrive once, on the first fragment for an index,
//     and are absent afterwards. They are set-if-empty and never overwritten with
//     an empty value.
//   - `arguments` is a raw JSON *string* fragment. It splits mid-token, mid-string
//     literal, and mid-escape sequence, so it is only ever concatenated — never
//     parsed, never trimmed — until the stream ends.
//   - Some providers omit `index` entirely; some send `arguments: ""` for a
//     zero-argument call and nothing else.

// ToolCallAccumulator reassembles tool calls from streamed fragments.
// It is not safe for concurrent use; one stream is consumed by one goroutine.
type ToolCallAccumulator struct {
	byIndex map[int]*accEntry
	order   []int
}

type accEntry struct {
	ID   string
	Type string
	Name string
	Args strings.Builder
}

// Add folds one chunk's tool-call fragments into the accumulator.
func (a *ToolCallAccumulator) Add(deltas []ToolCallDelta) {
	for _, d := range deltas {
		e := a.entryFor(d)

		// Set-if-empty. A provider that repeats the id on every fragment must not
		// cause it to be appended to itself, and one that sends an empty string
		// after the real value must not erase it.
		if e.ID == "" && d.ID != "" {
			e.ID = d.ID
		}
		if e.Type == "" && d.Type != "" {
			e.Type = d.Type
		}
		if d.Function != nil {
			if e.Name == "" && d.Function.Name != "" {
				e.Name = d.Function.Name
			}
			// Unconditional append, including empty fragments: a zero-argument call
			// may be signalled by a lone "" and nothing else.
			e.Args.WriteString(d.Function.Arguments)
		}
	}
}

// entryFor resolves which accumulating call a fragment belongs to.
func (a *ToolCallAccumulator) entryFor(d ToolCallDelta) *accEntry {
	if a.byIndex == nil {
		a.byIndex = make(map[int]*accEntry, 2)
	}

	idx := 0
	if d.Index != nil {
		idx = *d.Index
	} else if d.ID != "" {
		// No index. If we have already seen this id, continue that call; otherwise
		// fall back to slot 0, unless slot 0 is a *different* call — in which case
		// this is a second indexless call and needs its own slot. Merging them
		// would concatenate two unrelated argument strings into JSON garbage.
		if found, ok := a.indexOfID(d.ID); ok {
			idx = found
		} else if e, exists := a.byIndex[0]; exists && e.ID != "" && e.ID != d.ID {
			idx = a.nextFreeIndex()
		}
	}

	e, ok := a.byIndex[idx]
	if !ok {
		e = &accEntry{}
		a.byIndex[idx] = e
		a.order = append(a.order, idx)
	}
	return e
}

func (a *ToolCallAccumulator) indexOfID(id string) (int, bool) {
	for idx, e := range a.byIndex {
		if e.ID == id {
			return idx, true
		}
	}
	return 0, false
}

func (a *ToolCallAccumulator) nextFreeIndex() int {
	for i := 0; ; i++ {
		if _, taken := a.byIndex[i]; !taken {
			return i
		}
	}
}

// Len reports how many distinct tool calls have been seen.
func (a *ToolCallAccumulator) Len() int { return len(a.byIndex) }

// Finalize returns the completed tool calls, ordered by index.
//
// Ordering is by index rather than by arrival because providers that validate
// strictly expect the assistant message's tool_calls and the following tool
// result messages to correspond positionally.
func (a *ToolCallAccumulator) Finalize() []ToolCall {
	if len(a.byIndex) == 0 {
		return nil
	}

	indices := make([]int, 0, len(a.byIndex))
	for idx := range a.byIndex {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	out := make([]ToolCall, 0, len(indices))
	for _, idx := range indices {
		e := a.byIndex[idx]

		args := e.Args.String()
		if strings.TrimSpace(args) == "" {
			// A zero-argument call. Downstream unmarshals this, and "" is not JSON.
			args = "{}"
		}

		id := e.ID
		if id == "" {
			// Some providers omit ids in streaming mode, but the tool result message
			// must reference one or the next request is malformed. A synthesised id
			// is stable for this response, which is all that is required.
			id = "call_" + strconv.Itoa(idx)
		}

		typ := e.Type
		if typ == "" {
			typ = ToolTypeFunction
		}

		out = append(out, ToolCall{
			ID:       id,
			Type:     typ,
			Function: FunctionCall{Name: e.Name, Arguments: args},
		})
	}
	return out
}

// StreamAccumulator reassembles a whole streamed completion into the same shape a
// non-streaming call would have produced. The agent loop consumes streamed and
// non-streamed responses through one code path because of this.
type StreamAccumulator struct {
	ID                string
	Object            string
	Created           int64
	Model             string
	SystemFingerprint string

	Role         string
	FinishReason *string
	Usage        *Usage

	content   strings.Builder
	reasoning strings.Builder
	tools     ToolCallAccumulator

	chunks int
}

// Add folds one chunk into the accumulator.
func (s *StreamAccumulator) Add(c *ChatCompletionChunk) {
	if c == nil {
		return
	}
	s.chunks++

	// Identity fields arrive on the first chunk and are usually repeated. Keep the
	// first non-empty value: some providers blank them on the final usage-only chunk.
	if s.ID == "" {
		s.ID = c.ID
	}
	if s.Object == "" {
		s.Object = c.Object
	}
	if s.Created == 0 {
		s.Created = c.Created
	}
	if s.Model == "" {
		s.Model = c.Model
	}
	if s.SystemFingerprint == "" {
		s.SystemFingerprint = c.SystemFingerprint
	}

	// Usage generally arrives alone on a final chunk after the last content delta.
	if c.Usage != nil {
		s.Usage = c.Usage
	}

	for _, choice := range c.Choices {
		// Only choice 0 is meaningful here: the server rejects n > 1, since an agent
		// loop running n divergent tool-call histories has no coherent meaning.
		if choice.Index != 0 {
			continue
		}

		d := choice.Delta
		if d.Role != "" {
			s.Role = d.Role
		}
		if d.Content != nil {
			s.content.WriteString(*d.Content)
		}
		if d.ReasoningContent != nil {
			s.reasoning.WriteString(*d.ReasoningContent)
		}
		if len(d.ToolCalls) > 0 {
			s.tools.Add(d.ToolCalls)
		}

		// A finish reason can arrive on a chunk whose delta is otherwise empty, and
		// a later usage-only chunk may carry a null one. Keep the first real value.
		if choice.FinishReason != nil && s.FinishReason == nil {
			s.FinishReason = choice.FinishReason
		}
	}
}

// Chunks reports how many chunks were folded in, which distinguishes an empty
// response from a stream that never produced anything at all.
func (s *StreamAccumulator) Chunks() int { return s.chunks }

// Message returns the assembled assistant message.
func (s *StreamAccumulator) Message() Message {
	role := s.Role
	if role == "" {
		role = RoleAssistant
	}

	msg := Message{
		Role:             role,
		ReasoningContent: s.reasoning.String(),
		ToolCalls:        s.tools.Finalize(),
	}

	// An assistant message that only calls tools carries no text. Which of null,
	// "", and absent a provider will accept differs, so the neutral choice is made
	// here and the quirk layer adjusts it per provider.
	text := s.content.String()
	if text == "" && len(msg.ToolCalls) > 0 {
		msg.Content = NullContent()
	} else {
		msg.Content = TextContent(text)
	}

	return msg
}

// Response renders the accumulated stream as a non-streaming completion.
func (s *StreamAccumulator) Response() *ChatCompletionResponse {
	object := s.Object
	if object == "" {
		object = "chat.completion"
	} else if object == "chat.completion.chunk" {
		// The assembled whole is no longer a chunk.
		object = "chat.completion"
	}

	finish := s.FinishReason
	if finish == nil {
		// A stream that ended without one still has to report something; the tool
		// calls tell us which it was.
		reason := FinishStop
		if s.tools.Len() > 0 {
			reason = FinishToolCalls
		}
		finish = &reason
	}

	return &ChatCompletionResponse{
		ID:                s.ID,
		Object:            object,
		Created:           s.Created,
		Model:             s.Model,
		SystemFingerprint: s.SystemFingerprint,
		Usage:             s.Usage,
		Choices: []Choice{{
			Index:        0,
			Message:      s.Message(),
			FinishReason: finish,
		}},
	}
}
