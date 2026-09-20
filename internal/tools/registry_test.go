package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// fakeTool is a configurable Tool for exercising dispatch.
type fakeTool struct {
	name     string
	safe     bool
	panics   bool
	err      error
	out      any
	rendered string
}

func (f *fakeTool) Name() string        { return f.name }
func (f *fakeTool) Description() string { return "fake " + f.name }
func (f *fakeTool) Parameters(SchemaDialect) json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (f *fakeTool) ConcurrencySafe(json.RawMessage) bool { return f.safe }

func (f *fakeTool) Execute(context.Context, *workspace.Session, json.RawMessage) (any, error) {
	if f.panics {
		panic("deliberate panic in " + f.name)
	}
	return f.out, f.err
}

func (f *fakeTool) Render(json.RawMessage, any) string { return f.rendered }

func call(name, args string) oai.ToolCall {
	return oai.ToolCall{
		ID:       "call_" + name,
		Type:     oai.ToolTypeFunction,
		Function: oai.FunctionCall{Name: name, Arguments: args},
	}
}

func TestRegistryPreservesRegistrationOrder(t *testing.T) {
	// Stable order keeps the provider's prompt cache warm across turns; a map
	// iteration would reshuffle the tools array on every request.
	r := NewRegistry(&fakeTool{name: "read"}, &fakeTool{name: "glob"}, &fakeTool{name: "bash"})

	want := []string{"read", "glob", "bash"}
	if got := r.Names(); !equalStrings(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}

	defs := r.Definitions(DialectFull)
	for i, w := range want {
		if defs[i].Function.Name != w {
			t.Errorf("Definitions[%d] = %q, want %q", i, defs[i].Function.Name, w)
		}
		if defs[i].Type != oai.ToolTypeFunction {
			t.Errorf("Definitions[%d].Type = %q", i, defs[i].Type)
		}
	}
}

func TestRegistryRejectsDuplicateName(t *testing.T) {
	r := NewRegistry(&fakeTool{name: "read"})
	if err := r.Register(&fakeTool{name: "read"}); err == nil {
		t.Error("a duplicate tool name was accepted")
	}
}

func TestRegistrySubset(t *testing.T) {
	r := NewRegistry(&fakeTool{name: "read"}, &fakeTool{name: "glob"}, &fakeTool{name: "bash"})

	sub, err := r.Subset([]string{"glob", "read"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sub.Names(); !equalStrings(got, []string{"glob", "read"}) {
		t.Errorf("Subset order = %v, want the order requested", got)
	}
	if _, ok := sub.Get("bash"); ok {
		t.Error("a tool outside the subset is still reachable")
	}
	if r.Len() != 3 {
		t.Error("Subset mutated the original registry")
	}
}

func TestRegistrySubsetNamesTheAvailableTools(t *testing.T) {
	// A caller that misspells a tool name should be able to fix it from the error.
	r := NewRegistry(&fakeTool{name: "read"}, &fakeTool{name: "glob"})

	_, err := r.Subset([]string{"reed"})
	if err == nil {
		t.Fatal("an unknown tool name was accepted")
	}
	if !strings.Contains(err.Error(), "reed") {
		t.Errorf("the error does not name the unknown tool: %v", err)
	}
	if !strings.Contains(err.Error(), "read") || !strings.Contains(err.Error(), "glob") {
		t.Errorf("the error does not list what is available: %v", err)
	}
}

func TestInvokeUnknownToolReturnsAResultNotAnError(t *testing.T) {
	// A model that hallucinates a tool needs to be told what does exist, in the
	// conversation, so it can correct itself on the next turn.
	r := NewRegistry(&fakeTool{name: "read"}, &fakeTool{name: "glob"})
	s := newSession(t, nil)

	res := r.Invoke(context.Background(), s, call("search_files", "{}"))
	if !res.IsError {
		t.Fatal("an unknown tool did not produce an error result")
	}
	if res.Code != CodeUnknownTool {
		t.Errorf("Code = %q, want %q", res.Code, CodeUnknownTool)
	}
	if !strings.Contains(res.Content, "read") {
		t.Errorf("the result does not list the real tools: %q", res.Content)
	}
	if res.CallID != "call_search_files" {
		t.Errorf("CallID = %q — an error result must still be attributable", res.CallID)
	}
}

func TestInvokeContainsAPanic(t *testing.T) {
	// Concurrency-safe tools run in goroutines, where a panic is unrecoverable
	// by the handler that started them and takes the whole server down.
	r := NewRegistry(&fakeTool{name: "boom", panics: true})
	s := newSession(t, nil)

	res := r.Invoke(context.Background(), s, call("boom", "{}"))
	if !res.IsError || res.Code != CodePanic {
		t.Fatalf("res = %+v, want a contained TOOL_PANIC", res)
	}
	if res.CallID != "call_boom" {
		t.Errorf("CallID = %q, want it preserved through the recover", res.CallID)
	}
	if strings.Contains(res.Content, "goroutine") {
		t.Error("the stack trace leaked into the model-facing content")
	}
}

func TestInvokeCarriesToolErrorCodes(t *testing.T) {
	r := NewRegistry(&fakeTool{name: "t", err: Errorf(CodeNotFound, "nope.")})
	s := newSession(t, nil)

	res := r.Invoke(context.Background(), s, call("t", "{}"))
	if res.Code != CodeNotFound {
		t.Errorf("Code = %q, want %q", res.Code, CodeNotFound)
	}
}

func TestInvokeReportsCancellationAsSuch(t *testing.T) {
	r := NewRegistry(&fakeTool{name: "t", err: context.Canceled})
	s := newSession(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := r.Invoke(ctx, s, call("t", "{}"))
	if res.Code != CodeCancelled {
		t.Errorf("Code = %q, want %q", res.Code, CodeCancelled)
	}
}

func TestInvokeSuccessCarriesBothFormsOfTheResult(t *testing.T) {
	want := map[string]int{"n": 1}
	r := NewRegistry(&fakeTool{name: "t", out: want, rendered: "one thing"})
	s := newSession(t, nil)

	res := r.Invoke(context.Background(), s, call("t", "{}"))
	if res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if res.Content != "one thing" {
		t.Errorf("Content = %q", res.Content)
	}
	if res.Data == nil {
		t.Error("the structured form was dropped")
	}
}

func TestResultToMessageMarksErrors(t *testing.T) {
	// The tool message has nowhere to carry an error flag, so it goes in the
	// text. A model that cannot tell failure from success builds on the failure.
	ok := Result{CallID: "c1", Content: "fine"}.ToMessage()
	if ok.Role != oai.RoleTool || ok.ToolCallID != "c1" {
		t.Errorf("message = %+v", ok)
	}
	if ok.Content.String() != "fine" {
		t.Errorf("content = %q", ok.Content.String())
	}

	bad := Result{CallID: "c2", Content: "it broke", IsError: true}.ToMessage()
	if !strings.HasPrefix(bad.Content.String(), "Error:") {
		t.Errorf("an error result is indistinguishable from a success: %q", bad.Content.String())
	}
}

func TestRealToolsRegisterAndDescribeThemselves(t *testing.T) {
	r := NewRegistry(NewRead(), NewGlob())

	for _, d := range r.Definitions(DialectFull) {
		if d.Function.Description == "" {
			t.Errorf("%s has no description; it is prompt text and cannot be empty", d.Function.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(d.Function.Parameters, &schema); err != nil {
			t.Errorf("%s has an unparseable schema: %v", d.Function.Name, err)
		}
		if schema["type"] != "object" {
			t.Errorf("%s schema is not an object", d.Function.Name)
		}
	}
}

func TestRealToolSchemasStayInsideTheBasicDialect(t *testing.T) {
	// Several self-hosted runtimes compile the schema into a constrained decoder
	// and mishandle anything beyond flat primitives. Both dialects are checked
	// because a schema that only degrades correctly under one of them is a
	// provider-specific bug waiting to happen.
	forbidden := []string{"oneOf", "anyOf", "allOf", "$ref", "format"}

	for _, dialect := range []SchemaDialect{DialectFull, DialectBasic} {
		for _, d := range NewRegistry(NewRead(), NewGlob()).Definitions(dialect) {
			s := string(d.Function.Parameters)
			for _, key := range forbidden {
				if strings.Contains(s, `"`+key+`"`) {
					t.Errorf("%s schema uses %q under the %s dialect", d.Function.Name, key, dialect)
				}
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
