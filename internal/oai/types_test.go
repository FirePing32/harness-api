package oai

import (
	"encoding/json"
	"testing"
)

// roundTrip unmarshals into T and marshals back, returning the result as a
// generic map so tests can assert on keys without depending on field order.
func roundTrip[T any](t *testing.T, in string) (T, map[string]any) {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatalf("unmarshal: %v\ninput: %s", err, in)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("re-unmarshal: %v\noutput: %s", err, out)
	}
	return v, m
}

func TestMessagePreservesUnknownFields(t *testing.T) {
	// Providers invent fields constantly. Dropping one corrupts the conversation
	// several turns later, where it is nearly impossible to trace back.
	in := `{
		"role": "assistant",
		"content": "hi",
		"audio": {"id": "aud_1"},
		"some_future_field": [1, 2, 3]
	}`

	msg, out := roundTrip[Message](t, in)

	if msg.Role != RoleAssistant || msg.Content.String() != "hi" {
		t.Fatalf("known fields lost: %+v", msg)
	}
	if _, ok := out["audio"]; !ok {
		t.Error("unknown field \"audio\" was dropped")
	}
	if _, ok := out["some_future_field"]; !ok {
		t.Error("unknown field \"some_future_field\" was dropped")
	}
}

func TestMessageContentStates(t *testing.T) {
	// Providers disagree about what an assistant message with tool calls should
	// carry here, so all four states must survive a round trip distinctly.
	tests := []struct {
		name       string
		in         string
		wantKind   ContentKind
		wantKeySet bool
	}{
		{"absent", `{"role":"assistant","tool_calls":[]}`, ContentAbsent, false},
		{"null", `{"role":"assistant","content":null}`, ContentNull, true},
		{"empty string", `{"role":"assistant","content":""}`, ContentText, true},
		{"text", `{"role":"user","content":"hello"}`, ContentText, true},
		{"parts", `{"role":"user","content":[{"type":"text","text":"a"}]}`, ContentParts, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, out := roundTrip[Message](t, tt.in)
			if msg.Content.Kind != tt.wantKind {
				t.Errorf("Kind = %v, want %v", msg.Content.Kind, tt.wantKind)
			}
			_, present := out["content"]
			if present != tt.wantKeySet {
				t.Errorf("content key present = %v, want %v (output: %v)", present, tt.wantKeySet, out)
			}
		})
	}
}

func TestContentPartsFlattenToText(t *testing.T) {
	in := `{"role":"user","content":[
		{"type":"text","text":"look at "},
		{"type":"image_url","image_url":{"url":"data:..."}},
		{"type":"text","text":"this"}
	]}`

	msg, _ := roundTrip[Message](t, in)
	if got := msg.Content.String(); got != "look at this" {
		t.Errorf("String() = %q, want %q", got, "look at this")
	}
	if len(msg.Content.Parts) != 3 {
		t.Errorf("got %d parts, want 3 — non-text parts must survive even though "+
			"they contribute no text", len(msg.Content.Parts))
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	in := `{
		"role": "assistant",
		"content": null,
		"tool_calls": [
			{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"file_path\":\"/a\"}"}}
		]
	}`

	msg, out := roundTrip[Message](t, in)

	if len(msg.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "read" {
		t.Errorf("tool call mangled: %+v", tc)
	}
	// Arguments must stay a raw string. Parsing and re-encoding would reorder keys
	// and break any provider that hashes the call for caching.
	if tc.Function.Arguments != `{"file_path":"/a"}` {
		t.Errorf("Arguments = %q, want the original string verbatim", tc.Function.Arguments)
	}
	if out["content"] != nil {
		t.Errorf("content should marshal as null, got %v", out["content"])
	}
}

func TestReasoningContentIsCaptured(t *testing.T) {
	// Captured so clients can see it. Stripping for upstream happens in
	// internal/upstream, which is the only place that serialises for a provider.
	in := `{"role":"assistant","content":"answer","reasoning_content":"step 1..."}`

	msg, out := roundTrip[Message](t, in)
	if msg.ReasoningContent != "step 1..." {
		t.Errorf("ReasoningContent = %q, want it captured", msg.ReasoningContent)
	}
	if out["reasoning_content"] != "step 1..." {
		t.Errorf("reasoning_content should survive a plain round trip, got %v", out["reasoning_content"])
	}
}

func TestRequestPreservesUnknownFields(t *testing.T) {
	in := `{
		"model": "gpt-4o",
		"messages": [{"role":"user","content":"hi"}],
		"chat_template_kwargs": {"enable_thinking": true},
		"guided_json": {"type":"object"}
	}`

	req, out := roundTrip[ChatCompletionRequest](t, in)

	if req.Model != "gpt-4o" || len(req.Messages) != 1 {
		t.Fatalf("known fields lost: %+v", req)
	}
	// vLLM and friends take provider-specific knobs like these; a passthrough
	// server that eats them is useless to anyone tuning a local model.
	for _, k := range []string{"chat_template_kwargs", "guided_json"} {
		if _, ok := out[k]; !ok {
			t.Errorf("provider-specific field %q was dropped", k)
		}
	}
}

func TestOptionalScalarsDistinguishZeroFromAbsent(t *testing.T) {
	// temperature 0 is a real instruction. Sending it when the caller did not ask
	// for it changes the model's output.
	withZero, out := roundTrip[ChatCompletionRequest](t, `{"model":"m","messages":[],"temperature":0}`)
	if withZero.Temperature == nil || *withZero.Temperature != 0 {
		t.Fatalf("temperature 0 should survive, got %v", withZero.Temperature)
	}
	if _, ok := out["temperature"]; !ok {
		t.Error("temperature 0 must be re-emitted, not omitted as a zero value")
	}

	absent, out2 := roundTrip[ChatCompletionRequest](t, `{"model":"m","messages":[]}`)
	if absent.Temperature != nil {
		t.Errorf("absent temperature should stay nil, got %v", *absent.Temperature)
	}
	if _, ok := out2["temperature"]; ok {
		t.Error("absent temperature must not be invented on the way out")
	}
}

func TestHarnessExtensionParses(t *testing.T) {
	in := `{
		"model":"m",
		"messages":[],
		"harness":{"session_id":"sess_1","stream_events":true,"max_iterations":5}
	}`

	req, _ := roundTrip[ChatCompletionRequest](t, in)
	if req.Harness == nil {
		t.Fatal("harness extension not parsed")
	}
	if req.Harness.SessionID != "sess_1" || !req.Harness.StreamEvents {
		t.Errorf("harness extension mangled: %+v", req.Harness)
	}
	if req.Harness.MaxIterations == nil || *req.Harness.MaxIterations != 5 {
		t.Errorf("harness.max_iterations = %v, want 5", req.Harness.MaxIterations)
	}
	// It must not land in Extra as well, or it would be sent upstream by the
	// unknown-field passthrough after buildBody strips the typed field.
	if _, ok := req.Extra["harness"]; ok {
		t.Error("harness must be a known field, not captured as an unknown one")
	}
}

func TestFinishReasonDistinguishesNullFromEmpty(t *testing.T) {
	in := `{"id":"1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":null}]}`

	resp, _ := roundTrip[ChatCompletionResponse](t, in)
	if len(resp.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(resp.Choices))
	}
	if resp.Choices[0].FinishReason != nil {
		t.Errorf("null finish_reason should stay nil, got %q", *resp.Choices[0].FinishReason)
	}
}

func TestUsageAdd(t *testing.T) {
	// An agent request makes many internal calls; the client should be billed the
	// aggregate, not the last one.
	total := &Usage{}
	total.Add(&Usage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120})
	total.Add(&Usage{PromptTokens: 300, CompletionTokens: 50, TotalTokens: 350})
	total.Add(nil)

	if total.PromptTokens != 400 || total.CompletionTokens != 70 || total.TotalTokens != 470 {
		t.Errorf("aggregate = %+v, want 400/70/470", total)
	}
}

func TestKnownFieldsCoversEveryTaggedField(t *testing.T) {
	// The unknown-field capture depends on this set being complete. Deriving it by
	// reflection is what keeps it that way, so guard the derivation itself.
	for name, got := range map[string]map[string]struct{}{
		"Message":               messageKnown,
		"ChatCompletionRequest": chatRequestKnown,
	} {
		if len(got) == 0 {
			t.Errorf("%s: knownFields returned nothing", name)
		}
	}
	for _, want := range []string{"role", "content", "tool_calls", "tool_call_id", "reasoning_content"} {
		if _, ok := messageKnown[want]; !ok {
			t.Errorf("messageKnown is missing %q", want)
		}
	}
	if _, ok := messageKnown["-"]; ok {
		t.Error("json:\"-\" fields must not be treated as known keys")
	}
}
