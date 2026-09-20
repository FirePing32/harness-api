// Package logx configures structured logging and scrubs credentials.
//
// Everything this server touches is adjacent to a secret: upstream API keys ride
// in request headers, and some providers echo the entire request (key included)
// back inside their error bodies. Redaction therefore happens at the handler, not
// at each call site, so that forgetting to scrub is not a possible mistake.
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// Options configures the root logger.
type Options struct {
	Level  string // debug | info | warn | error
	Format string // json | text
	Writer io.Writer
}

// New builds the root logger. An unrecognised level falls back to info, and an
// unrecognised format falls back to text; bad logging config should never be the
// reason a server refuses to boot.
func New(opts Options) *slog.Logger {
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}

	handlerOpts := &slog.HandlerOptions{Level: parseLevel(opts.Level)}

	var h slog.Handler
	switch strings.ToLower(opts.Format) {
	case "json":
		h = slog.NewJSONHandler(w, handlerOpts)
	default:
		h = slog.NewTextHandler(w, handlerOpts)
	}

	return slog.New(&redactingHandler{inner: h})
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// sensitiveKeys are attribute names whose values are replaced wholesale. Matching
// is case-insensitive and substring-based, so "upstream_api_key" is caught by "api_key".
var sensitiveKeys = []string{
	"authorization",
	"api_key",
	"apikey",
	"token",
	"secret",
	"password",
	"credential",
	"x-api-key",
}

// secretPattern catches credentials embedded in free-form text — upstream error
// bodies are the main offender, since several providers echo the request they
// received, Authorization header and all.
var secretPattern = regexp.MustCompile(
	`(?i)\b(sk-[A-Za-z0-9_\-]{8,}|Bearer\s+[A-Za-z0-9._\-]{8,}|(?:api[_-]?key|token)["'\s:=]+[A-Za-z0-9._\-]{8,})`,
)

const redacted = "[REDACTED]"

// Redact scrubs credential-shaped substrings from arbitrary text. Exported because
// upstream error bodies need scrubbing before they are attached as log attributes.
func Redact(s string) string {
	return secretPattern.ReplaceAllString(s, redacted)
}

func isSensitive(key string) bool {
	k := strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// redactingHandler wraps another handler and rewrites attributes on the way through.
type redactingHandler struct {
	inner slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	clone := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		clone.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, clone)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return &redactingHandler{inner: h.inner.WithAttrs(out)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if isSensitive(a.Key) {
		return slog.String(a.Key, redacted)
	}

	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindGroup:
		attrs := v.Group()
		out := make([]any, 0, len(attrs))
		for _, g := range attrs {
			out = append(out, redactAttr(g))
		}
		return slog.Group(a.Key, out...)
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
