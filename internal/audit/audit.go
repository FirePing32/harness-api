// Package audit records the security-relevant things this server did.
//
// It is deliberately not the operational log. That one is levelled, sampled in
// places, and routinely turned down to warn in production — all reasonable for
// diagnostics and all fatal for a record you may need to reconstruct months
// later. An audit trail that disappears when someone quietens the logs is not
// an audit trail.
//
// So it is a separate sink, one JSON object per line, written and flushed as
// each event happens. No buffering: the events most worth having are the ones
// immediately before something went badly wrong, which is exactly when a
// buffer is lost.
//
// # Commands are recorded verbatim, and that has a cost
//
// A command can contain a credential — `curl -H "Authorization: Bearer sk-..."`
// is a thing models write. Everywhere else in this server, credentials are
// redacted in the log handler so that leaking one is not a possible mistake.
// Here they are not, because a record of *approximately* what ran cannot answer
// the question an audit log exists to answer.
//
// The consequence is that the audit file is as sensitive as the secrets that
// pass through it. It is created 0600 and should stay that way; do not ship it
// to a log aggregator without thinking about what is in it.
package audit

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Kind classifies an audit event.
type Kind string

const (
	// KindCommand is a shell command that ran.
	KindCommand Kind = "command"
	// KindDenied is a tool call a guard refused.
	KindDenied Kind = "denied"
	// KindSession is a session created or destroyed.
	KindSession Kind = "session"
)

// Event is one audited action.
type Event struct {
	Time string `json:"time"`
	Kind Kind   `json:"kind"`

	SessionID string `json:"session_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`

	// Command is the shell command, exactly as it ran. See the package comment.
	Command string `json:"command,omitempty"`
	Workdir string `json:"workdir,omitempty"`

	ExitCode   *int   `json:"exit_code,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Limits     string `json:"limits,omitempty"`

	// Tool, Guard and Reason describe a refusal.
	Tool   string `json:"tool,omitempty"`
	Guard  string `json:"guard,omitempty"`
	Reason string `json:"reason,omitempty"`

	// Action describes a session event: "created" or "destroyed".
	Action string `json:"action,omitempty"`
}

// Logger writes audit events.
//
// The nil Logger is valid and discards everything, so callers never have to
// check. Auditing is off unless an operator asks for it, and a call site that
// has to guard against nil is a call site that will eventually forget.
type Logger struct {
	mu   sync.Mutex
	w    io.Writer
	c    io.Closer
	path string

	// onError reports a failed write once. A full disk should not turn every
	// subsequent command into a torrent of identical log lines.
	onError func(error)
	failed  bool
}

// Open starts an audit log at path, creating it if needed and appending
// otherwise. An empty path returns nil, which discards.
func Open(path string, onError func(error)) (*Logger, error) {
	if path == "" {
		return nil, nil
	}

	// 0600 because of what goes in it. O_APPEND so that concurrent writes of
	// whole lines do not interleave, and so a restart never truncates history.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening the audit log: %w", err)
	}
	return &Logger{w: f, c: f, path: path, onError: onError}, nil
}

// NewWriter builds a Logger over an arbitrary writer, for tests.
func NewWriter(w io.Writer) *Logger { return &Logger{w: w} }

// Path is where events are being written, empty when auditing is off.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Enabled reports whether anything is recorded.
func (l *Logger) Enabled() bool { return l != nil && l.w != nil }

// Close releases the file.
func (l *Logger) Close() error {
	if l == nil || l.c == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.c.Close()
}

// Write records one event. It never returns an error: an audit write that
// fails must not fail the request that triggered it, because that would turn a
// full disk into an outage and give anyone who wanted the audit trail off a way
// to switch it off.
func (l *Logger) Write(ev Event) {
	if !l.Enabled() {
		return
	}
	if ev.Time == "" {
		ev.Time = time.Now().UTC().Format(time.RFC3339Nano)
	}

	line, err := json.Marshal(ev)
	if err != nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(append(line, '\n')); err != nil && !l.failed {
		l.failed = true
		if l.onError != nil {
			l.onError(err)
		}
	}
}

// Command records a shell execution.
func (l *Logger) Command(sessionID, workspace, command, workdir string,
	exitCode int, timedOut bool, duration time.Duration, limits string,
) {
	l.Write(Event{
		Kind: KindCommand, SessionID: sessionID, Workspace: workspace,
		Command: command, Workdir: workdir,
		ExitCode: &exitCode, TimedOut: timedOut,
		DurationMS: duration.Milliseconds(), Limits: limits,
	})
}

// Denied records a tool call a guard refused.
func (l *Logger) Denied(sessionID, tool, guard, reason string) {
	l.Write(Event{
		Kind: KindDenied, SessionID: sessionID,
		Tool: tool, Guard: guard, Reason: reason,
	})
}

// Session records a session being created or destroyed.
func (l *Logger) Session(sessionID, workspace, action string) {
	l.Write(Event{
		Kind: KindSession, SessionID: sessionID,
		Workspace: workspace, Action: action,
	})
}

// ErrorReporter builds an onError callback that complains to a slog logger.
func ErrorReporter(log *slog.Logger) func(error) {
	return func(err error) {
		log.Error("the audit log could not be written; auditing is now incomplete",
			"error", err)
	}
}
