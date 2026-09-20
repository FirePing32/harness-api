package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/prakhargurunani/harness-api/internal/oai"
)

type ctxKey int

const ctxKeyRequestID ctxKey = iota

// RequestIDFromContext returns the id assigned to this request, or "" if called
// outside a request.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKeyRequestID).(string)
	return id
}

type middleware func(http.Handler) http.Handler

// chain applies middleware so that the first argument is the outermost wrapper,
// which is the order they read in at the call site.
func chain(h http.Handler, ms ...middleware) http.Handler {
	for i := len(ms) - 1; i >= 0; i-- {
		h = ms[i](h)
	}
	return h
}

// withRequestID stamps every request with an id, echoes it in a header, and makes
// it available to handlers. Client-facing errors are deliberately vague about
// internals, so this id is the only way to correlate a user report with the log.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not worth failing a request over; ids are for
		// correlation, not security.
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// withRecover turns a panic into a 500 rather than a dropped connection. An agent
// loop touches a lot of provider-shaped data; a nil map somewhere should cost one
// request, not the process.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				reqID := RequestIDFromContext(r.Context())
				s.log.Error("panic serving request",
					"request_id", reqID, "path", r.URL.Path, "panic", v)
				oai.WriteError(w, oai.NewAPIError("internal server error", nil), reqID)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status = http.StatusOK
		w.wrote = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so streaming responses are not buffered
// by the access-log wrapper.
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"request_id", RequestIDFromContext(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start))
	})
}

// withAuth enforces bearer-token authentication.
//
// When no tokens are configured the check is skipped entirely. That is only
// reachable on a loopback bind — config.Validate refuses to start an exposed
// listener without tokens — so it cannot silently leave a network-facing server
// open.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens := s.cfg.Server.AuthTokens
		if len(tokens) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		presented := bearerToken(r)
		if presented == "" {
			oai.WriteError(w, oai.NewAuthError(
				"missing bearer token; pass Authorization: Bearer <token>"),
				RequestIDFromContext(r.Context()))
			return
		}

		// Every token is compared, and comparison is constant-time, so neither the
		// match position nor the token contents leak through timing.
		var ok bool
		for _, want := range tokens {
			if subtle.ConstantTimeCompare([]byte(presented), []byte(want)) == 1 {
				ok = true
			}
		}
		if !ok {
			oai.WriteError(w, oai.NewAuthError("invalid bearer token"),
				RequestIDFromContext(r.Context()))
			return
		}

		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// withBodyLimit caps request bodies. Without it, a single request can make the
// server allocate without bound.
func (s *Server) withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}
