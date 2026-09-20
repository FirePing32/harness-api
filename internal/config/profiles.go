package config

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Provider quirk profiles.
//
// "OpenAI-compatible" is a description of a shape, not a specification. Every
// provider implements a slightly different subset and rejects a slightly
// different set of fields, and the disagreements are not exotic — they are
// about the token-limit parameter, the name of the system role, and whether an
// assistant message carrying tool calls should have null content, empty
// content, or no content key at all.
//
// A profile is data, not code. There are more providers than anyone will write
// structs for, so the compatibility layer is a set of transforms selected by
// configuration, and a provider nobody has heard of is handled by overriding
// fields rather than by a patch.
//
// **Provenance.** These profiles are assembled from provider documentation and
// from error messages reported in the wild. They have not each been verified
// against a live endpoint. Where a profile here and a real provider disagree,
// the provider is right — fix the profile, and prefer `upstream.profile_overrides`
// over waiting for a release. The autodetect layer in internal/upstream exists
// precisely because this list will be wrong somewhere.

// ContentStyle is how an assistant message carrying tool calls represents its
// (usually empty) content. Providers disagree, and sending the wrong one is a
// 400 on the *next* request rather than this one, which makes it unusually
// annoying to diagnose.
type ContentStyle string

const (
	// StyleNull sends "content": null. The OpenAI default.
	StyleNull ContentStyle = "null"
	// StyleEmpty sends "content": "". Several self-hosted runtimes require a
	// string and reject null.
	StyleEmpty ContentStyle = "empty"
	// StyleOmit leaves the key out entirely.
	StyleOmit ContentStyle = "omit"
	// StylePassthrough sends whatever arrived, changing nothing.
	StylePassthrough ContentStyle = ""
)

// Schema dialects, mirroring tools.SchemaDialect. Kept as a string here so
// that configuration does not depend on the tools package.
const (
	DialectFull  = "full"
	DialectBasic = "basic"
)

// Profile is one provider's deviations from the OpenAI request shape.
//
// Every field is expressed so that the zero value is the conservative choice,
// because an unknown provider gets the zero value.
type Profile struct {
	Name string `json:"name"`

	// MaxTokensField is which parameter carries the output limit:
	// "max_tokens" or "max_completion_tokens".
	MaxTokensField string `json:"max_tokens_field"`

	// SystemRole is what a system message's role becomes: "system",
	// "developer", or "user" to fold it into the first user message for
	// providers with no system role at all.
	SystemRole string `json:"system_role"`

	// ToolCallContent is how an empty assistant content is represented when
	// the message carries tool calls.
	ToolCallContent ContentStyle `json:"tool_call_content"`

	// ParallelToolCalls reports whether the field may be sent. Several
	// providers 400 on an unrecognised key rather than ignoring it.
	ParallelToolCalls bool `json:"parallel_tool_calls"`

	// StrictSchemas reports whether function definitions may carry "strict".
	StrictSchemas bool `json:"strict_schemas"`

	// Sampling reports whether temperature, top_p and the penalties may be
	// sent. Reasoning models generally reject them outright.
	Sampling bool `json:"sampling"`

	// MaxStopSequences caps the stop array. Zero means no limit.
	MaxStopSequences int `json:"max_stop_sequences"`

	// SchemaDialect is how much JSON Schema the provider can parse.
	SchemaDialect string `json:"schema_dialect"`

	// StripThinkTags removes <think>…</think> from response content. Some
	// providers emit reasoning inline instead of in reasoning_content, and
	// leaving it in means the model's scratchpad reaches the end user.
	StripThinkTags bool `json:"strip_think_tags"`

	// StreamFormat forces the stream framing: "", "sse" or "ndjson". Empty
	// sniffs it.
	StreamFormat string `json:"stream_format"`

	// StreamUsage reports whether stream_options.include_usage is understood.
	StreamUsage bool `json:"stream_usage"`

	// ContextWindow is the model's total token budget, used by compaction.
	// Zero means unknown.
	ContextWindow int `json:"context_window"`
}

// builtinProfiles are the named starting points.
//
// generic is deliberately the most restrictive: it sends only what every
// OpenAI-compatible implementation has been observed to accept. Being too
// conservative costs a feature; being too liberal costs a 400 that looks like
// the model is broken.
var builtinProfiles = map[string]Profile{
	"generic": {
		Name:             "generic",
		MaxTokensField:   "max_tokens",
		SystemRole:       "system",
		ToolCallContent:  StyleNull,
		Sampling:         true,
		SchemaDialect:    DialectBasic,
		MaxStopSequences: 4,
	},

	"openai": {
		Name:              "openai",
		MaxTokensField:    "max_completion_tokens",
		SystemRole:        "system",
		ToolCallContent:   StyleNull,
		ParallelToolCalls: true,
		StrictSchemas:     true,
		Sampling:          true,
		MaxStopSequences:  4,
		SchemaDialect:     DialectFull,
		StreamUsage:       true,
		ContextWindow:     128000,
	},

	// The o-series and other reasoning models: no sampling parameters, and the
	// system role was renamed.
	"openai-reasoning": {
		Name:              "openai-reasoning",
		MaxTokensField:    "max_completion_tokens",
		SystemRole:        "developer",
		ToolCallContent:   StyleNull,
		ParallelToolCalls: true,
		StrictSchemas:     true,
		Sampling:          false,
		MaxStopSequences:  4,
		SchemaDialect:     DialectFull,
		StreamUsage:       true,
		ContextWindow:     200000,
	},

	"deepseek": {
		Name:              "deepseek",
		MaxTokensField:    "max_tokens",
		SystemRole:        "system",
		ToolCallContent:   StyleNull,
		ParallelToolCalls: true,
		Sampling:          true,
		MaxStopSequences:  4,
		SchemaDialect:     DialectFull,
		StreamUsage:       true,
		ContextWindow:     65536,
	},

	// The reasoner rejects sampling parameters, and reasoning_content must
	// never be echoed back — that part is enforced unconditionally in
	// internal/upstream, for every profile, because it is a 400 rather than a
	// degradation.
	"deepseek-reasoner": {
		Name:             "deepseek-reasoner",
		MaxTokensField:   "max_tokens",
		SystemRole:       "system",
		ToolCallContent:  StyleNull,
		Sampling:         false,
		MaxStopSequences: 4,
		SchemaDialect:    DialectFull,
		StreamUsage:      true,
		ContextWindow:    65536,
	},

	"anthropic-compat": {
		Name:             "anthropic-compat",
		MaxTokensField:   "max_tokens",
		SystemRole:       "system",
		ToolCallContent:  StyleOmit,
		Sampling:         true,
		MaxStopSequences: 4,
		SchemaDialect:    DialectFull,
		ContextWindow:    200000,
	},

	"groq": {
		Name:             "groq",
		MaxTokensField:   "max_tokens",
		SystemRole:       "system",
		ToolCallContent:  StyleNull,
		Sampling:         true,
		MaxStopSequences: 4,
		SchemaDialect:    DialectFull,
		StreamUsage:      true,
		ContextWindow:    131072,
	},

	"together": {
		Name:             "together",
		MaxTokensField:   "max_tokens",
		SystemRole:       "system",
		ToolCallContent:  StyleNull,
		Sampling:         true,
		MaxStopSequences: 4,
		SchemaDialect:    DialectBasic,
		ContextWindow:    32768,
	},

	// Self-hosted runtimes compile the tool schema into a constrained decoder,
	// so anything beyond flat primitives is either rejected or silently
	// mishandled — which surfaces as the model emitting arguments that do not
	// match the schema.
	"vllm": {
		Name:            "vllm",
		MaxTokensField:  "max_tokens",
		SystemRole:      "system",
		ToolCallContent: StyleEmpty,
		Sampling:        true,
		SchemaDialect:   DialectBasic,
		StripThinkTags:  true,
		ContextWindow:   32768,
	},

	"ollama": {
		Name:            "ollama",
		MaxTokensField:  "max_tokens",
		SystemRole:      "system",
		ToolCallContent: StyleEmpty,
		Sampling:        true,
		SchemaDialect:   DialectBasic,
		StripThinkTags:  true,
		ContextWindow:   8192,
	},
}

// ProfileNames lists the built-in profiles, sorted.
func ProfileNames() []string {
	out := make([]string, 0, len(builtinProfiles))
	for name := range builtinProfiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// LookupProfile returns a built-in profile by name.
func LookupProfile(name string) (Profile, bool) {
	p, ok := builtinProfiles[name]
	return p, ok
}

// ResolveProfile returns the configured profile with any per-field overrides
// applied.
//
// Overrides are a partial JSON object merged over the named profile, so a
// provider nobody has written a profile for is a config change rather than a
// code change. That is the difference between this layer being usable and
// being a list somebody has to maintain.
func (c *Config) ResolveProfile() (Profile, error) {
	name := c.Upstream.Profile
	if name == "" {
		name = "generic"
	}

	p, ok := LookupProfile(name)
	if !ok {
		return Profile{}, fmt.Errorf(
			"unknown upstream profile %q; available: %v", name, ProfileNames())
	}

	if len(c.Upstream.ProfileOverrides) > 0 {
		if err := json.Unmarshal(c.Upstream.ProfileOverrides, &p); err != nil {
			return Profile{}, fmt.Errorf("upstream.profile_overrides: %w", err)
		}
		p.Name = name + "+overrides"
	}
	return p, nil
}
