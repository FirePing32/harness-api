package agent

import (
	"fmt"
	"strings"

	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// The system prompt is assembled from ordered sections rather than written as
// one block, for two reasons.
//
// The first is that the environment part changes per session — workspace path,
// what is in it — while the instruction part never does. Keeping them apart
// means the fixed prefix is byte-identical across requests, which is what
// provider-side prompt caching keys on. One interpolated path near the top
// invalidates the cache for the whole prompt.
//
// The second is that a caller's own system message has to be merged with ours
// without either silently winning. Sections make the precedence explicit.

// promptSection is one titled block of the system prompt.
type promptSection struct {
	title string
	body  string
}

// identity comes first and never varies.
const identityPrompt = `You are a coding agent working in a real filesystem workspace. You investigate before you act, make the smallest change that solves the problem, and verify your work.`

// toolGuidance is fixed text about how to use the tools well. It stays separate
// from each tool's own description: that describes one tool in isolation, this
// is about choosing between them.
const toolGuidance = `Working effectively:

- Look before you change anything. Use glob to find files by name and grep to find them by content. Reading a whole file to locate one function wastes the context you will need later.
- Read a file before editing it. This is enforced, not advice: an edit to a file you have not read will be refused, and so will an edit to a file that changed after you read it.
- Prefer edit over write. Rewriting a whole file to change a few lines loses anything you did not reproduce exactly, and you will not notice.
- When an edit fails, read the error. It tells you what the file actually contains at that point, including whitespace. Retrying the same edit unchanged will fail the same way.
- Make independent read-only calls together in one turn rather than one at a time.
- If a task cannot be done, say so and explain why. Do not substitute something easier and present it as the answer.`

// responseGuidance shapes the final answer.
const responseGuidance = `When you are finished, say what you changed and where, briefly. Do not repeat file contents the user can see, and do not narrate the steps you took unless something surprising happened.`

// Build assembles the system prompt for a session.
//
// The caller's own system messages are appended last, under a heading that
// marks them as instructions from the user. Putting them last means they win
// where they conflict with ours, which is the behaviour a caller expects when
// they set a system prompt; marking them keeps the boundary legible rather
// than letting caller text read as though this server wrote it.
func Build(s *workspace.Session, toolNames []string, callerSystem []string) string {
	sections := []promptSection{
		{body: identityPrompt},
		{title: "Tools", body: toolGuidance},
		{title: "Response", body: responseGuidance},
		{title: "Environment", body: environmentSection(s, toolNames)},
	}

	if len(callerSystem) > 0 {
		sections = append(sections, promptSection{
			title: "Instructions from the user",
			body:  strings.Join(callerSystem, "\n\n"),
		})
	}

	var b strings.Builder
	for i, sec := range sections {
		if i > 0 {
			b.WriteString("\n\n")
		}
		if sec.title != "" {
			b.WriteString("# ")
			b.WriteString(sec.title)
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimSpace(sec.body))
	}
	return b.String()
}

// environmentSection describes the workspace. Deliberately short: a long
// directory listing here is paid for on every single turn of the loop, and the
// model can ask for one with glob if it wants it.
func environmentSection(s *workspace.Session, toolNames []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace root: %s\n", s.Root())
	fmt.Fprintf(&b, "Paths in tool calls are relative to this root. You cannot read or write outside it.\n")
	if len(toolNames) > 0 {
		fmt.Fprintf(&b, "Available tools: %s\n", strings.Join(toolNames, ", "))
	}
	return b.String()
}

// SplitSystem separates leading system and developer messages from the rest of
// the conversation.
//
// They are extracted wherever they appear, not just at the front: clients that
// re-send a conversation sometimes interleave them, and a system message in the
// middle of a history is treated by most providers as an instruction rather
// than as part of the dialogue.
func SplitSystem(messages []oai.Message) (system []string, rest []oai.Message) {
	for _, m := range messages {
		switch m.Role {
		case oai.RoleSystem, oai.RoleDeveloper:
			if text := strings.TrimSpace(m.Content.String()); text != "" {
				system = append(system, text)
			}
		default:
			rest = append(rest, m)
		}
	}
	return system, rest
}
