package oai

import "testing"

func idx(i int) *int { return &i }

func str(s string) *string { return &s }

// fn builds a tool-call fragment.
func fn(index *int, id, name, args string) ToolCallDelta {
	d := ToolCallDelta{Index: index, ID: id}
	if name != "" || args != "" {
		d.Function = &FunctionCallDelta{Name: name, Arguments: args}
	}
	return d
}

func TestAccumulatorRepeatedIDIsNotAppended(t *testing.T) {
	// Some providers repeat id and name on every fragment. Appending would produce
	// "call_1call_1call_1"; only the arguments are ever concatenated.
	var a ToolCallAccumulator
	a.Add([]ToolCallDelta{fn(idx(0), "call_1", "read", `{"a`)})
	a.Add([]ToolCallDelta{fn(idx(0), "call_1", "read", `":1}`)})

	calls := a.Finalize()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].ID != "call_1" {
		t.Errorf("ID = %q, want call_1 (not repeated)", calls[0].ID)
	}
	if calls[0].Function.Name != "read" {
		t.Errorf("Name = %q, want read (not repeated)", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("Arguments = %q, want the fragments joined", calls[0].Function.Arguments)
	}
}

func TestAccumulatorEmptyValuesDoNotEraseRealOnes(t *testing.T) {
	// A fragment carrying id:"" after the real id must not blank it out.
	var a ToolCallAccumulator
	a.Add([]ToolCallDelta{fn(idx(0), "call_real", "grep", "")})
	a.Add([]ToolCallDelta{fn(idx(0), "", "", `{"p":1}`)})

	calls := a.Finalize()
	if calls[0].ID != "call_real" || calls[0].Function.Name != "grep" {
		t.Errorf("identity erased by a later empty fragment: %+v", calls[0])
	}
}

func TestAccumulatorIndexZeroIsDistinctFromAbsent(t *testing.T) {
	// omitempty on the provider's side makes index 0 vanish. If absent were
	// treated as "not zero" these two fragments would become separate calls.
	var a ToolCallAccumulator
	a.Add([]ToolCallDelta{fn(idx(0), "call_x", "read", `{"a`)})
	a.Add([]ToolCallDelta{fn(nil, "call_x", "", `":1}`)})

	calls := a.Finalize()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 — an absent index for a known id continues it", len(calls))
	}
	if calls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("Arguments = %q", calls[0].Function.Arguments)
	}
}

func TestAccumulatorOrdersByIndexNotArrival(t *testing.T) {
	// Providers may emit indices out of order. Strict providers expect the
	// assistant's tool_calls and the following tool results to correspond
	// positionally, so output order must be by index.
	var a ToolCallAccumulator
	a.Add([]ToolCallDelta{fn(idx(2), "call_c", "third", "{}")})
	a.Add([]ToolCallDelta{fn(idx(0), "call_a", "first", "{}")})
	a.Add([]ToolCallDelta{fn(idx(1), "call_b", "second", "{}")})

	calls := a.Finalize()
	want := []string{"first", "second", "third"}
	if len(calls) != 3 {
		t.Fatalf("got %d calls, want 3", len(calls))
	}
	for i, w := range want {
		if calls[i].Function.Name != w {
			t.Errorf("position %d = %q, want %q", i, calls[i].Function.Name, w)
		}
	}
}

func TestAccumulatorHandlesIndexGaps(t *testing.T) {
	// A gap must not produce a phantom entry for the missing index.
	var a ToolCallAccumulator
	a.Add([]ToolCallDelta{fn(idx(0), "call_a", "first", "{}")})
	a.Add([]ToolCallDelta{fn(idx(5), "call_f", "sixth", "{}")})

	calls := a.Finalize()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 — gaps are not filled with empties", len(calls))
	}
}

func TestAccumulatorSeparatesIndexlessCallsByID(t *testing.T) {
	// Two indexless calls with different ids must not merge, which would splice
	// their argument strings into JSON garbage.
	var a ToolCallAccumulator
	a.Add([]ToolCallDelta{fn(nil, "call_a", "read", `{"x":1}`)})
	a.Add([]ToolCallDelta{fn(nil, "call_b", "glob", `{"y":2}`)})

	calls := a.Finalize()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	for _, c := range calls {
		if c.Function.Arguments != `{"x":1}` && c.Function.Arguments != `{"y":2}` {
			t.Errorf("arguments spliced across calls: %q", c.Function.Arguments)
		}
	}
}

func TestAccumulatorMultipleFragmentsInOneChunk(t *testing.T) {
	// A single chunk may carry fragments for several indices at once.
	var a ToolCallAccumulator
	a.Add([]ToolCallDelta{
		fn(idx(0), "call_a", "read", `{"a":1}`),
		fn(idx(1), "call_b", "grep", `{"b":2}`),
	})

	if a.Len() != 2 {
		t.Fatalf("Len = %d, want 2", a.Len())
	}
}

func TestAccumulatorSynthesisesMissingID(t *testing.T) {
	// Some providers omit ids while streaming, but the tool result message must
	// reference one or the next request is malformed.
	var a ToolCallAccumulator
	a.Add([]ToolCallDelta{fn(idx(0), "", "read", `{}`)})

	calls := a.Finalize()
	if calls[0].ID == "" {
		t.Error("a missing id must be synthesised, not left empty")
	}
	if calls[0].Type != ToolTypeFunction {
		t.Errorf("Type = %q, want a default of %q", calls[0].Type, ToolTypeFunction)
	}
}

func TestAccumulatorEmptyIsNil(t *testing.T) {
	var a ToolCallAccumulator
	if got := a.Finalize(); got != nil {
		t.Errorf("Finalize on an untouched accumulator = %v, want nil", got)
	}
}

func TestStreamAccumulatorKeepsFirstFinishReason(t *testing.T) {
	// The real reason arrives on one chunk; a trailing usage-only chunk carries a
	// null one that must not overwrite it.
	var s StreamAccumulator
	s.Add(&ChatCompletionChunk{
		Choices: []ChunkChoice{{Index: 0, Delta: Delta{Content: str("hi")}}},
	})
	s.Add(&ChatCompletionChunk{
		Choices: []ChunkChoice{{Index: 0, FinishReason: str(FinishStop)}},
	})
	s.Add(&ChatCompletionChunk{
		Choices: []ChunkChoice{},
		Usage:   &Usage{TotalTokens: 9},
	})

	if s.FinishReason == nil || *s.FinishReason != FinishStop {
		t.Errorf("FinishReason = %v, want stop", s.FinishReason)
	}
	if s.Usage == nil || s.Usage.TotalTokens != 9 {
		t.Errorf("usage from the trailing chunk lost: %+v", s.Usage)
	}
}

func TestStreamAccumulatorIgnoresNonZeroChoices(t *testing.T) {
	// n > 1 is rejected at the handler; anything that slips through must not
	// contaminate choice 0's content.
	var s StreamAccumulator
	s.Add(&ChatCompletionChunk{Choices: []ChunkChoice{
		{Index: 0, Delta: Delta{Content: str("keep")}},
		{Index: 1, Delta: Delta{Content: str("DROP")}},
	}})

	if got := s.Message().Content.String(); got != "keep" {
		t.Errorf("content = %q, want only choice 0", got)
	}
}

func TestStreamAccumulatorSurvivesNilChunk(t *testing.T) {
	var s StreamAccumulator
	s.Add(nil)
	if s.Chunks() != 0 {
		t.Errorf("a nil chunk should not count, got %d", s.Chunks())
	}
}

func TestStreamAccumulatorInfersFinishReason(t *testing.T) {
	// A stream that ends without a finish reason still has to report one, and the
	// presence of tool calls says which.
	var withTools StreamAccumulator
	withTools.Add(&ChatCompletionChunk{Choices: []ChunkChoice{{
		Index: 0,
		Delta: Delta{ToolCalls: []ToolCallDelta{fn(idx(0), "c1", "read", "{}")}},
	}}})
	if got := *withTools.Response().Choices[0].FinishReason; got != FinishToolCalls {
		t.Errorf("finish_reason = %q, want %q", got, FinishToolCalls)
	}

	var textOnly StreamAccumulator
	textOnly.Add(&ChatCompletionChunk{Choices: []ChunkChoice{{
		Index: 0, Delta: Delta{Content: str("done")},
	}}})
	if got := *textOnly.Response().Choices[0].FinishReason; got != FinishStop {
		t.Errorf("finish_reason = %q, want %q", got, FinishStop)
	}
}

func TestStreamAccumulatorContentKindForToolOnlyMessage(t *testing.T) {
	var s StreamAccumulator
	s.Add(&ChatCompletionChunk{Choices: []ChunkChoice{{
		Index: 0,
		Delta: Delta{ToolCalls: []ToolCallDelta{fn(idx(0), "c1", "read", "{}")}},
	}}})

	// Null, not "": providers disagree, and the quirk layer adjusts from here.
	if got := s.Message().Content.Kind; got != ContentNull {
		t.Errorf("Kind = %v, want ContentNull for a tools-only message", got)
	}

	// But a message with real text keeps it, tool calls or not.
	var withText StreamAccumulator
	withText.Add(&ChatCompletionChunk{Choices: []ChunkChoice{{
		Index: 0,
		Delta: Delta{
			Content:   str("calling a tool"),
			ToolCalls: []ToolCallDelta{fn(idx(0), "c1", "read", "{}")},
		},
	}}})
	if got := withText.Message().Content.String(); got != "calling a tool" {
		t.Errorf("content = %q", got)
	}
}
