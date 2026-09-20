package upstream

import (
	"regexp"
	"strings"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/oai"
)

// Apply rewrites a request into the shape one provider will accept.
//
// It never mutates the caller's request. The agent loop holds the running
// history and re-sends it every turn, so a transform that edited in place
// would compound: a system message renamed to "developer" on turn one would
// be renamed again on turn two, and folding a system message into a user
// message would duplicate it once per turn.
func Apply(p config.Profile, req *oai.ChatCompletionRequest) *oai.ChatCompletionRequest {
	out := *req
	out.Messages = applyMessages(p, req.Messages)
	out.Tools = applyTools(p, req.Tools)

	applyTokenLimit(p, &out)

	if !p.Sampling {
		// Reasoning models reject these outright rather than ignoring them, so
		// a request that merely passed temperature through fails entirely.
		out.Temperature = nil
		out.TopP = nil
		out.PresencePenalty = nil
		out.FrequencyPenalty = nil
	}

	if !p.ParallelToolCalls {
		out.ParallelToolCalls = nil
	}

	if n := p.MaxStopSequences; n > 0 && len(out.Stop) > n {
		out.Stop = out.Stop[:n]
	}

	if !p.StreamUsage {
		out.StreamOptions = nil
	}

	return &out
}

// applyTokenLimit moves the output limit into whichever field this provider
// reads, so a caller can set either and have it work.
func applyTokenLimit(p config.Profile, req *oai.ChatCompletionRequest) {
	limit := req.MaxCompletionTokens
	if limit == nil {
		limit = req.MaxTokens
	}
	if limit == nil {
		return
	}

	req.MaxTokens = nil
	req.MaxCompletionTokens = nil
	if p.MaxTokensField == "max_completion_tokens" {
		req.MaxCompletionTokens = limit
	} else {
		req.MaxTokens = limit
	}
}

// applyMessages rewrites roles and content styles.
func applyMessages(p config.Profile, in []oai.Message) []oai.Message {
	if len(in) == 0 {
		return in
	}

	out := make([]oai.Message, 0, len(in))
	var folded []string

	for _, m := range in {
		if m.Role == oai.RoleSystem || m.Role == oai.RoleDeveloper {
			switch p.SystemRole {
			case oai.RoleDeveloper:
				m.Role = oai.RoleDeveloper
			case oai.RoleUser:
				// No system role at all. The text has to survive somewhere, so
				// it is folded into the first user message rather than dropped:
				// losing it silently would remove the tool instructions and
				// leave the model guessing at how to work.
				if text := strings.TrimSpace(m.Content.String()); text != "" {
					folded = append(folded, text)
				}
				continue
			default:
				m.Role = oai.RoleSystem
			}
		}

		if m.Role == oai.RoleAssistant && len(m.ToolCalls) > 0 {
			m.Content = toolCallContent(p, m.Content)
		}
		out = append(out, m)
	}

	if len(folded) > 0 {
		out = foldIntoFirstUser(out, strings.Join(folded, "\n\n"))
	}
	return out
}

// toolCallContent adjusts the content of an assistant message that carries
// tool calls, but only when there is no real text to preserve.
func toolCallContent(p config.Profile, c oai.Content) oai.Content {
	if strings.TrimSpace(c.String()) != "" {
		return c
	}
	switch p.ToolCallContent {
	case config.StyleNull:
		return oai.NullContent()
	case config.StyleEmpty:
		return oai.TextContent("")
	case config.StyleOmit:
		return oai.Content{Kind: oai.ContentAbsent}
	default:
		return c
	}
}

// foldIntoFirstUser prepends text to the first user message, or inserts one.
func foldIntoFirstUser(msgs []oai.Message, text string) []oai.Message {
	for i, m := range msgs {
		if m.Role != oai.RoleUser {
			continue
		}
		msgs[i].Content = oai.TextContent(text + "\n\n" + m.Content.String())
		return msgs
	}
	return append([]oai.Message{{
		Role: oai.RoleUser, Content: oai.TextContent(text),
	}}, msgs...)
}

// applyTools drops function-definition fields the provider rejects.
func applyTools(p config.Profile, in []oai.Tool) []oai.Tool {
	if len(in) == 0 || p.StrictSchemas {
		return in
	}
	out := make([]oai.Tool, len(in))
	copy(out, in)
	for i := range out {
		out[i].Function.Strict = nil
	}
	return out
}

// thinkTag matches an inline reasoning block.
//
// Some providers put the model's scratchpad in the content rather than in
// reasoning_content. Left alone it reaches the end user, and worse, it goes
// back upstream as part of the assistant turn on the next iteration, where the
// model treats its own discarded thinking as settled fact.
var thinkTag = regexp.MustCompile(`(?is)<(think|thinking)>.*?</(think|thinking)>\s*`)

// StripThink removes inline reasoning blocks from a response, moving the first
// one into ReasoningContent so nothing is lost outright.
func StripThink(p config.Profile, resp *oai.ChatCompletionResponse) {
	if !p.StripThinkTags || resp == nil {
		return
	}
	for i := range resp.Choices {
		msg := &resp.Choices[i].Message
		text := msg.Content.String()
		if !thinkTag.MatchString(text) {
			continue
		}
		if msg.ReasoningContent == "" {
			msg.ReasoningContent = strings.TrimSpace(
				stripTagMarkers(thinkTag.FindString(text)))
		}
		msg.Content = oai.TextContent(strings.TrimSpace(thinkTag.ReplaceAllString(text, "")))
	}
}

var tagMarkers = regexp.MustCompile(`(?i)</?(think|thinking)>`)

func stripTagMarkers(s string) string { return tagMarkers.ReplaceAllString(s, "") }

// Learning a provider's rules from its complaints.
//
// A wrong profile is otherwise a hard failure with a confusing message: the
// caller sees a 400 about a parameter they never set, and nothing in it
// suggests that the fix is a configuration name. Autodetect turns that into a
// slower success plus a log line telling the operator exactly what to pin.
//
// It is capped, and the cap matters. Without one, a provider that rejects
// something we cannot infer produces an unbounded retry loop that looks like a
// hang and costs real money. Three attempts is enough for the combinations
// seen in practice — usually a token field and a role name together.

// Fix names a single inferred adjustment.
type Fix string

// autoFix pairs a provider complaint with the profile change it implies.
type autoFix struct {
	Name  Fix
	Match *regexp.Regexp
	Apply func(*config.Profile)
}

// autoFixes are ordered by how specific the pattern is: an error naming a
// parameter is matched before one naming only a role.
var autoFixes = []autoFix{
	{
		Name: "max_completion_tokens",
		Match: regexp.MustCompile(`(?i)` +
			`(max_tokens.{0,80}(not supported|unsupported|deprecated|use\s+'?max_completion_tokens)` +
			`|unsupported\s+parameter:?\s*'?max_tokens)`),
		Apply: func(p *config.Profile) { p.MaxTokensField = "max_completion_tokens" },
	},
	{
		Name: "max_tokens",
		Match: regexp.MustCompile(`(?i)` +
			`(unsupported|unrecognized|unknown|invalid).{0,40}'?max_completion_tokens`),
		Apply: func(p *config.Profile) { p.MaxTokensField = "max_tokens" },
	},
	{
		Name: "system-role",
		Match: regexp.MustCompile(`(?i)` +
			`'?developer'?\s+is\s+not\s+(a\s+)?valid|invalid\s+role.{0,20}developer` +
			`|role.{0,20}'?developer'?.{0,30}(not supported|unsupported)`),
		Apply: func(p *config.Profile) { p.SystemRole = "system" },
	},
	{
		Name: "developer-role",
		Match: regexp.MustCompile(`(?i)` +
			`'?system'?\s+is\s+not\s+(a\s+)?valid|use\s+'?developer'?\s+role` +
			`|role.{0,20}'?system'?.{0,30}(not supported|unsupported)`),
		Apply: func(p *config.Profile) { p.SystemRole = "developer" },
	},
	{
		Name: "no-parallel-tool-calls",
		Match: regexp.MustCompile(`(?i)` +
			`(unsupported|unrecognized|unknown|invalid|extra).{0,40}'?parallel_tool_calls`),
		Apply: func(p *config.Profile) { p.ParallelToolCalls = false },
	},
	{
		Name: "no-sampling",
		Match: regexp.MustCompile(`(?i)` +
			`(unsupported|does not support|not supported).{0,40}'?(temperature|top_p)`),
		Apply: func(p *config.Profile) { p.Sampling = false },
	},
	{
		Name: "no-strict",
		Match: regexp.MustCompile(`(?i)` +
			`(unsupported|unrecognized|unknown|invalid|extra).{0,40}'?strict`),
		Apply: func(p *config.Profile) { p.StrictSchemas = false },
	},
	{
		Name: "basic-schema",
		Match: regexp.MustCompile(`(?i)` +
			`(unsupported|unknown|invalid).{0,40}(oneOf|anyOf|allOf|\$ref|schema keyword)`),
		Apply: func(p *config.Profile) { p.SchemaDialect = config.DialectBasic },
	},
	{
		Name: "no-stream-usage",
		Match: regexp.MustCompile(`(?i)` +
			`(unsupported|unrecognized|unknown|invalid|extra).{0,40}'?stream_options`),
		Apply: func(p *config.Profile) { p.StreamUsage = false },
	},
}

// inferFix finds the adjustment a provider's rejection implies.
func inferFix(body string) (autoFix, bool) {
	for _, f := range autoFixes {
		if f.Match.MatchString(body) {
			return f, true
		}
	}
	return autoFix{}, false
}
