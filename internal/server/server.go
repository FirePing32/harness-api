// Package server wires HTTP routes onto the agent machinery.
package server

import (
	"log/slog"
	"net/http"

	"github.com/FirePing32/harness-api/internal/agent"
	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/tools"
	"github.com/FirePing32/harness-api/internal/upstream"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// Server owns the route table. It is constructed once at startup; the returned
// handler is safe for concurrent use.
type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	upstream *upstream.Client
	sessions *workspace.Manager
	agent    *agent.Loop
	mux      *http.ServeMux
}

// DefaultTools is the tool set offered to the model.
//
// The order is fixed and load-bearing: it is the order definitions are sent in,
// and a stable array is what keeps provider-side prompt caching effective from
// one turn to the next.
func DefaultTools() *tools.Registry {
	return tools.NewRegistry(
		tools.NewRead(),
		tools.NewGlob(),
		tools.NewGrep(),
		tools.NewEdit(),
		tools.NewWrite(),
	)
}

// New builds a Server and registers its routes.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	sessions, err := workspace.NewManager(cfg.Workspace, log)
	if err != nil {
		return nil, err
	}

	client := upstream.New(cfg.Upstream, log)
	s := &Server{
		cfg:      cfg,
		log:      log,
		upstream: client,
		sessions: sessions,
		agent: agent.New(agent.Options{
			Upstream: client,
			Registry: DefaultTools(),
			Config:   cfg.Agent,
			Log:      log,
		}),
		mux: http.NewServeMux(),
	}
	s.routes()
	return s, nil
}

// Sessions exposes the session registry so the entrypoint can run the idle
// sweeper and release workspaces at shutdown.
func (s *Server) Sessions() *workspace.Manager { return s.sessions }

func (s *Server) routes() {
	// Health endpoints sit outside /v1 and outside auth: a liveness probe should
	// not need a credential, and it reveals nothing.
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /readyz", s.handleReadyz)

	v1 := http.NewServeMux()
	v1.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	v1.HandleFunc("GET /v1/models", s.handleModels)

	s.mux.Handle("/v1/", chain(v1, s.withAuth, s.withBodyLimit))
}

// Handler returns the fully wrapped handler, middleware included.
func (s *Server) Handler() http.Handler {
	return chain(s.mux,
		withRequestID,   // outermost: everything below can log a correlation id
		s.withRecover,   // catch panics before the access log records a bogus 200
		s.withAccessLog, // time the real work, not the recovery
	)
}
