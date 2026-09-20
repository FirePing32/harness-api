// Package tools implements the capabilities the agent exposes to the model.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/FirePing32/harness-api/internal/workspace"
)

// Tool is one capability offered to the model.
//
// Execute returns structured data and Render turns that data into the text the
// model sees. Keeping them apart costs one extra method and buys three things:
// the structured form can be streamed to a harness-aware UI without reparsing
// prose, replay of a recorded run is deterministic because rendering is pure,
// and the wording shown to the model can be tuned per provider without
// touching the code that does the work.
type Tool interface {
	Name() string

	// Description is sent to the model verbatim. It is prompt text, and it is
	// load-bearing: most tool misuse is a description problem.
	Description() string

	// Parameters is the JSON Schema for the arguments, adjusted for what the
	// target provider can actually parse.
	Parameters(dialect SchemaDialect) json.RawMessage

	// ConcurrencySafe reports whether this call may run alongside others. Only
	// an explicit true opts in; anything unsure is treated as exclusive and
	// becomes an ordering barrier.
	ConcurrencySafe(args json.RawMessage) bool

	Execute(ctx context.Context, s *workspace.Session, args json.RawMessage) (any, error)

	// Render is pure: same arguments and result, same text, no I/O.
	Render(args json.RawMessage, result any) string
}

// SchemaDialect selects how much JSON Schema the provider can be trusted with.
type SchemaDialect string

const (
	// DialectFull permits the whole vocabulary: oneOf, $ref, format, enums.
	DialectFull SchemaDialect = "full"

	// DialectBasic is flat objects with primitive typed properties and nothing
	// else. Several self-hosted runtimes compile the schema into a constrained
	// decoder and reject or silently mis-handle anything more, which surfaces as
	// the model emitting arguments that do not match what was asked for.
	DialectBasic SchemaDialect = "basic"
)

// Error is a tool failure the model is expected to recover from.
//
// These are never returned to the client as HTTP errors. They go back as an
// ordinary tool result, because the text is the model's recovery signal: it is
// the only thing standing between a wrong call and the retry that fixes it.
// That makes the wording part of the implementation, not decoration. Every
// message here says what was wrong and what to do next.
type Error struct {
	Code    string
	Message string
	// Hint is an optional concrete next action, rendered on its own line.
	Hint string
}

func (e *Error) Error() string {
	if e.Hint == "" {
		return e.Message
	}
	return e.Message + "\n" + e.Hint
}

// Tool error codes.
const (
	CodeInvalidArgs = "INVALID_ARGUMENTS"
	CodeNotFound    = "NOT_FOUND"
	CodeIsDirectory = "IS_DIRECTORY"
	CodeNotRegular  = "NOT_A_REGULAR_FILE"
	CodeBinary      = "BINARY_FILE"
	CodeTooLarge    = "TOO_LARGE"
	CodeIO          = "IO_ERROR"
	CodeUnknownTool = "UNKNOWN_TOOL"
	CodePanic       = "TOOL_PANIC"
	CodeCancelled   = "CANCELLED"

	// CodeNoMatch and CodeAmbiguous are the two edit failures the model is
	// expected to recover from on its own, and the ones whose error text does
	// the most work. They are tracked separately in evals: a high rate of either
	// is a signal about the error messages, not about the model.
	CodeNoMatch   = "NO_MATCH"
	CodeAmbiguous = "AMBIGUOUS_MATCH"
)

// Errorf builds a tool Error.
func Errorf(code, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// WithHint attaches a suggested next action.
func (e *Error) WithHint(format string, a ...any) *Error {
	e.Hint = fmt.Sprintf(format, a...)
	return e
}

// Result is one finished tool call.
type Result struct {
	// CallID ties the result back to the model's tool call.
	CallID string `json:"call_id,omitempty"`
	Tool   string `json:"tool"`

	// Content is what the model sees.
	Content string `json:"content"`

	// Data is the structured form, for event streaming and for tests that would
	// otherwise have to parse prose.
	Data any `json:"data,omitempty"`

	IsError bool   `json:"is_error,omitempty"`
	Code    string `json:"code,omitempty"`
}

// codeOf extracts a tool error code from any error, falling back to a generic
// one so a result is never left with an empty code.
func codeOf(err error) string {
	var te *Error
	if errors.As(err, &te) {
		return te.Code
	}
	var pe *workspace.PathError
	if errors.As(err, &pe) {
		return pe.Code
	}
	var oe *workspace.ObservationError
	if errors.As(err, &oe) {
		return oe.Code
	}
	return CodeIO
}

// parseArgs decodes a tool's arguments, rejecting keys the tool does not know.
//
// Being strict here is a deliberate choice. A model that invents a plausible
// parameter — "recursive", "encoding", "max_results" — and has it silently
// dropped gets a result that ignores an instruction it believes it gave, and
// the mismatch is invisible. Naming the unknown key gets it corrected on the
// next call.
func parseArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return Errorf(CodeInvalidArgs, "could not read the arguments: %s", cleanJSONError(err))
	}
	return nil
}

// cleanJSONError strips the Go type names out of a decoding error. "cannot
// unmarshal string into Go struct field readArgs.offset of type int" tells the
// model about our structs; "offset must be a number" tells it what to fix.
func cleanJSONError(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "Go struct field "); i >= 0 {
		rest := msg[i+len("Go struct field "):]
		field, typ, ok := strings.Cut(rest, " of type ")
		if ok {
			if _, name, found := strings.Cut(field, "."); found {
				field = name
			}
			return fmt.Sprintf("%s must be of type %s", field, typ)
		}
	}
	msg = strings.TrimPrefix(msg, "json: ")
	return msg
}

// errorAs is errors.As, re-exported for tests in this package.
func errorAs(err error, target any) bool { return errors.As(err, target) }
