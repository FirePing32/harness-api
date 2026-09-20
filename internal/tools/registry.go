package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// Registry maps tool names to implementations and dispatches calls.
// Safe for concurrent use: read tools run in parallel.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Tool
	order  []string
}

// NewRegistry builds a registry. Registering a duplicate name panics, because
// it can only be a programming error at startup.
func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		if err := r.Register(t); err != nil {
			panic(err)
		}
	}
	return r
}

// Register adds a tool.
func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if name == "" {
		return fmt.Errorf("tool has no name")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("tool %q is already registered", name)
	}
	r.byName[name] = t
	r.order = append(r.order, name)
	return nil
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// Names lists the registered tools in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.order...)
}

// Len reports how many tools are registered.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byName)
}

// Subset returns a registry holding only the named tools, preserving the order
// given. Used for the per-request tool allowlist.
func (r *Registry) Subset(names []string) (*Registry, error) {
	out := &Registry{byName: make(map[string]Tool, len(names))}
	var unknown []string

	r.mu.RLock()
	for _, n := range names {
		t, ok := r.byName[n]
		if !ok {
			unknown = append(unknown, n)
			continue
		}
		if _, dup := out.byName[n]; dup {
			continue
		}
		out.byName[n] = t
		out.order = append(out.order, n)
	}
	available := append([]string(nil), r.order...)
	r.mu.RUnlock()

	if len(unknown) > 0 {
		sort.Strings(available)
		return nil, fmt.Errorf("unknown tool(s) %s; available: %s",
			strings.Join(unknown, ", "), strings.Join(available, ", "))
	}
	return out, nil
}

// Definitions renders the registry as the tools array of a chat completion
// request, in registration order. Order is stable so that prompt caching on
// the provider side is not defeated by a map iteration.
func (r *Registry) Definitions(dialect SchemaDialect) []oai.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]oai.Tool, 0, len(r.order))
	for _, name := range r.order {
		t := r.byName[name]
		out = append(out, oai.Tool{
			Type: oai.ToolTypeFunction,
			Function: oai.FunctionDef{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Parameters(dialect),
			},
		})
	}
	return out
}

// Invoke runs one tool call and always returns a Result. It does not return an
// error: a failed tool call is a normal outcome that the model reads and acts
// on, and turning it into a transport error would end the conversation at
// exactly the moment the model was about to fix it.
func (r *Registry) Invoke(ctx context.Context, s *workspace.Session, call oai.ToolCall) (res Result) {
	name := call.Function.Name
	res = Result{CallID: call.ID, Tool: name}

	t, ok := r.Get(name)
	if !ok {
		available := strings.Join(r.Names(), ", ")
		return errorResult(res, Errorf(CodeUnknownTool,
			"there is no tool called %q.", name).WithHint("Available tools: %s", available))
	}

	// A panicking tool must not take the process with it. Tool calls run in
	// goroutines when they are concurrency-safe, and a panic in a goroutine is
	// unrecoverable by the request handler that started it.
	defer func() {
		if p := recover(); p != nil {
			res = errorResult(Result{CallID: call.ID, Tool: name}, Errorf(CodePanic,
				"the %s tool failed unexpectedly and the call was abandoned.", name).
				WithHint("Try a different approach, or a different tool."))
			res.Data = map[string]string{"panic": fmt.Sprint(p), "stack": string(debug.Stack())}
		}
	}()

	args := json.RawMessage(call.Function.Arguments)
	out, err := t.Execute(ctx, s, args)
	if err != nil {
		if ctx.Err() != nil {
			return errorResult(res, Errorf(CodeCancelled,
				"the %s call was stopped before it finished.", name))
		}
		return errorResult(res, err)
	}

	res.Content = t.Render(args, out)
	res.Data = out
	return res
}

func errorResult(res Result, err error) Result {
	res.IsError = true
	res.Code = codeOf(err)
	res.Content = err.Error()
	return res
}

// ToMessage renders a Result as the tool message that goes back to the model.
//
// The error flag is carried in the content rather than a separate field: the
// OpenAI tool message has nowhere to put it, and a model that cannot tell a
// failure from a successful result will build on the failure.
func (res Result) ToMessage() oai.Message {
	content := res.Content
	if res.IsError {
		content = "Error: " + content
	}
	return oai.Message{
		Role:       oai.RoleTool,
		Content:    oai.TextContent(content),
		ToolCallID: res.CallID,
	}
}
