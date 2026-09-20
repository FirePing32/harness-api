// Package oai defines the OpenAI chat-completions wire schema.
//
// One schema serves both directions: we accept it from clients and we send it to
// whichever OpenAI-compatible provider backs the agent. That is deliberate. The
// agent loop's dominant operation is appending to a message history and re-sending
// it, so a second parallel type set would mean converting the entire history on
// every turn — O(n) churn per iteration, and one more place for a newly-added
// field to be silently dropped mid-conversation. History fidelity is the single
// most correctness-critical property of an agent loop, so the conversion is
// removed rather than made careful.
//
// The two directions differ in exactly two ways, both handled without a type split:
//   - Inbound-only extensions live in the Harness field, which is deleted at the
//     one outbound serialisation chokepoint (upstream.Client).
//   - Provider variance is expressed as transforms over a request (see
//     internal/upstream/quirks.go), not as types. There are N providers; there
//     cannot be N structs.
package oai

import "encoding/json"

// Roles recognised in a conversation.
const (
	RoleSystem    = "system"
	RoleDeveloper = "developer"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Finish reasons.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// ToolTypeFunction is the only tool type we emit.
const ToolTypeFunction = "function"

// ChatCompletionRequest is POST /v1/chat/completions.
//
// Optional scalars are pointers because zero and absent are different requests:
// temperature 0 is a meaningful instruction, and sending it when the caller did
// not ask for it changes the model's behaviour.
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`

	Tools             []Tool `json:"tools,omitempty"`
	ToolChoice        any    `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool  `json:"parallel_tool_calls,omitempty"`

	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	N                *int     `json:"n,omitempty"`

	// Both token limits are modelled because providers split on which they accept;
	// the quirk layer decides which one actually goes on the wire.
	MaxTokens           *int `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`

	Stop            []string        `json:"stop,omitempty"`
	ResponseFormat  json.RawMessage `json:"response_format,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`

	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	User string `json:"user,omitempty"`

	// Harness carries this server's extensions. It is inbound-only and is stripped
	// before any upstream request is sent.
	Harness *HarnessRequestExt `json:"harness,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// StreamOptions mirrors OpenAI's stream_options object.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// HarnessRequestExt is this server's non-standard request extension. Every field
// is optional; a request that omits the whole object behaves like a plain
// OpenAI call against an ephemeral workspace.
type HarnessRequestExt struct {
	// SessionID reattaches to an existing session and its workspace.
	SessionID string `json:"session_id,omitempty"`
	// Workspace names a directory to work in, creating a session bound to it.
	Workspace string `json:"workspace,omitempty"`
	// StreamEvents opts into structured tool-activity deltas (see agent streaming).
	StreamEvents bool `json:"stream_events,omitempty"`
	// MaxIterations overrides the server's per-request loop ceiling, downward only.
	MaxIterations *int `json:"max_iterations,omitempty"`
	// Tools restricts the agent to a named subset of the built-in tools.
	Tools []string `json:"tools,omitempty"`
}

var chatRequestKnown = knownFields(ChatCompletionRequest{})

type chatRequestAlias ChatCompletionRequest

func (r *ChatCompletionRequest) UnmarshalJSON(b []byte) error {
	var a chatRequestAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*r = ChatCompletionRequest(a)

	extra, err := captureExtra(b, chatRequestKnown)
	if err != nil {
		return err
	}
	r.Extra = extra
	return nil
}

func (r ChatCompletionRequest) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(chatRequestAlias(r))
	if err != nil {
		return nil, err
	}
	return mergeExtra(b, r.Extra)
}

// Message is one conversation turn.
type Message struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`

	Name string `json:"name,omitempty"`

	// ToolCalls is set on assistant messages that invoke tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set on tool-result messages and must echo the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`

	// ReasoningContent holds chain-of-thought that reasoning models return out of
	// band. It is captured so clients can see it, but it must never be sent back
	// upstream — DeepSeek rejects requests that echo it. The strip happens in
	// internal/upstream, which is the only place that serialises for a provider.
	ReasoningContent string `json:"reasoning_content,omitempty"`

	Refusal *string `json:"refusal,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var messageKnown = knownFields(Message{})

type messageAlias Message

func (m *Message) UnmarshalJSON(b []byte) error {
	var a messageAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*m = Message(a)

	// A message object with no "content" key is distinct from one with a null
	// content, and only the raw bytes can tell us which we got.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	if _, present := probe["content"]; !present {
		m.Content = Content{Kind: ContentAbsent}
	}

	extra, err := captureExtra(b, messageKnown)
	if err != nil {
		return err
	}
	m.Extra = extra
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(messageAlias(m))
	if err != nil {
		return nil, err
	}
	var omit []string
	if m.Content.Kind == ContentAbsent {
		omit = append(omit, "content")
	}
	return mergeExtra(b, m.Extra, omit...)
}

// ToolCall is a function invocation requested by the model.
type ToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Index appears in streaming deltas and identifies which call a fragment
	// belongs to. It is absent in complete (non-streaming) tool calls.
	Index    *int         `json:"index,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is a tool call's name and its raw JSON argument string. Arguments
// stays a string rather than a parsed object because that is how it arrives, in
// fragments, and premature parsing is the classic streaming bug.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a tool offered to the model.
type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a callable function.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// Strict enables structured-output schema enforcement where supported. It is a
	// pointer so the quirk layer can drop it for providers that reject the key.
	Strict *bool `json:"strict,omitempty"`
}

// ChatCompletionResponse is a non-streaming completion.
type ChatCompletionResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`

	// Harness reports what the server did, notably which session handled the request.
	Harness *HarnessResponseExt `json:"harness,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

// HarnessResponseExt is this server's non-standard response extension.
type HarnessResponseExt struct {
	SessionID  string `json:"session_id,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
	Iterations int    `json:"iterations,omitempty"`
	ToolCalls  int    `json:"tool_calls,omitempty"`
	Compacted  int    `json:"compacted,omitempty"`
}

var chatResponseKnown = knownFields(ChatCompletionResponse{})

type chatResponseAlias ChatCompletionResponse

func (r *ChatCompletionResponse) UnmarshalJSON(b []byte) error {
	var a chatResponseAlias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*r = ChatCompletionResponse(a)

	extra, err := captureExtra(b, chatResponseKnown)
	if err != nil {
		return err
	}
	r.Extra = extra
	return nil
}

func (r ChatCompletionResponse) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(chatResponseAlias(r))
	if err != nil {
		return nil, err
	}
	return mergeExtra(b, r.Extra)
}

// Choice is one completion alternative.
type Choice struct {
	Index   int     `json:"index"`
	Message Message `json:"message"`
	// FinishReason is a pointer because providers send null while a choice is still
	// in flight, and null is meaningfully different from the empty string.
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs,omitempty"`
}

// Usage is token accounting. For an agent request this is the aggregate across
// every internal model call the loop made, not just the last one.
type Usage struct {
	PromptTokens            int             `json:"prompt_tokens"`
	CompletionTokens        int             `json:"completion_tokens"`
	TotalTokens             int             `json:"total_tokens"`
	PromptTokensDetails     json.RawMessage `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails json.RawMessage `json:"completion_tokens_details,omitempty"`
}

// Add accumulates another usage record into u.
func (u *Usage) Add(other *Usage) {
	if other == nil {
		return
	}
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
}

// ChatCompletionChunk is one server-sent event in a streaming completion.
type ChatCompletionChunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	Choices           []ChunkChoice `json:"choices"`
	Usage             *Usage        `json:"usage,omitempty"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
}

// ChunkChoice is one choice's slice of a streamed chunk.
type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the incremental payload of a streamed chunk. Content is a pointer
// because an empty-string delta and an absent delta both occur and mean different
// things: the former is a real (if empty) token event, the latter is a chunk
// carrying only a finish reason or usage.
type Delta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
	Refusal          *string         `json:"refusal,omitempty"`

	// Harness carries structured tool-activity events when a client opts in.
	// Standard SDKs ignore unknown delta keys, so this is invisible to them.
	Harness json.RawMessage `json:"harness,omitempty"`
}

// ToolCallDelta is a fragment of a tool call.
type ToolCallDelta struct {
	// Index identifies which tool call this fragment belongs to. It is a pointer
	// because index 0 is real and omitempty on the provider's side makes it vanish;
	// treating absent as "not zero" would merge two distinct calls.
	Index    *int               `json:"index,omitempty"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function *FunctionCallDelta `json:"function,omitempty"`
}

// FunctionCallDelta is a fragment of a tool call's name and arguments.
type FunctionCallDelta struct {
	Name string `json:"name,omitempty"`
	// Arguments is a fragment of a JSON string. It splits mid-token, mid-string
	// literal, and mid-escape-sequence; it is only ever concatenated, never parsed,
	// until the stream ends.
	Arguments string `json:"arguments,omitempty"`
}
