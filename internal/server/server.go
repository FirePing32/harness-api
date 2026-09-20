// Package server wires HTTP routes onto the agent machinery.
package server

import (
	"log/slog"
	"net/http"

	"github.com/FirePing32/harness-api/internal/agent"
	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/contextmgr"
	"github.com/FirePing32/harness-api/internal/guard"
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
//
// bash is offered only when it is enabled. Registering it and having it refuse
// every call would waste a slot in the model's attention and invite it to keep
// trying; leaving it out means the model plans around the tools it has.
func DefaultTools(cfg *config.Config) *tools.Registry {
	r := tools.NewRegistry(
		tools.NewRead(),
		tools.NewGlob(),
		tools.NewGrep(),
		tools.NewEdit(),
		tools.NewWrite(),
	)
	if cfg.Shell.Enabled {
		if err := r.Register(tools.NewBash(workspace.NewLocalShell(), cfg.Shell)); err != nil {
			panic(err)
		}
	}
	return r
}

// DefaultGuards builds the guard chain from configuration.
//
// Order affects only which message a denied call gets back, never whether it
// is denied: guards cannot permit, so no later guard can undo an earlier
// refusal. Cheap checks come first so the expensive ones are rarely reached.
func DefaultGuards(cfg *config.Config) (*guard.Chain, []error) {
	chain := guard.NewChain()
	if !cfg.Guards.Enabled {
		return chain, nil
	}

	chain.Add(guard.RepeatTool(cfg.Guards.RepeatThreshold))

	if cfg.Shell.Enabled {
		patterns := cfg.Guards.DenyCommands
		if patterns == nil {
			patterns = guard.DefaultDeniedCommands
		}
		denylist, errs := guard.CommandDenylist(patterns)
		chain.Add(denylist)
		chain.Add(guard.TimeoutPolicy())
		return chain, errs
	}

	return chain, nil
}

// New builds a Server and registers its routes.
func New(cfg *config.Config, log *slog.Logger) (*Server, error) {
	sessions, err := workspace.NewManager(cfg.Workspace, log)
	if err != nil {
		return nil, err
	}

	guards, guardErrs := DefaultGuards(cfg)
	for _, err := range guardErrs {
		// A bad pattern narrows what is blocked rather than stopping the server,
		// so it is a warning. Silence would leave an operator believing a rule
		// they wrote is in force when it never compiled.
		log.Warn("guard configuration ignored", "error", err)
	}
	if guards.Len() > 0 {
		log.Info("tool guards active", "guards", guards.Names())
	}

	profile, err := cfg.ResolveProfile()
	if err != nil {
		return nil, err
	}
	log.Info("provider profile",
		"profile", profile.Name,
		"max_tokens_field", profile.MaxTokensField,
		"system_role", profile.SystemRole,
		"schema_dialect", profile.SchemaDialect)

	client := upstream.NewWithProfile(cfg.Upstream, profile, log)

	var compactor *contextmgr.Compactor
	if cfg.Context.Enabled {
		window := cfg.Context.Window
		if window == 0 {
			window = profile.ContextWindow
		}
		if window == 0 {
			// No window is knowable, so compaction is off rather than guessing.
			// A wrong guess either wastes a summarisation call on every request
			// or fails to prevent the overflow it exists for.
			log.Warn("context compaction disabled: no context window is known for " +
				"this profile; set context.window to enable it")
		} else {
			compactor = contextmgr.New(contextmgr.Options{
				Summarizer: client,
				Log:        log,
				Window:     window,
				Threshold:  cfg.Context.Threshold,
				Retain:     cfg.Context.Retain,
			})
			log.Info("context compaction enabled",
				"window", window,
				"threshold", cfg.Context.Threshold,
				"retain", cfg.Context.Retain)
		}
	}

	s := &Server{
		cfg:      cfg,
		log:      log,
		upstream: client,
		sessions: sessions,
		agent: agent.New(agent.Options{
			Upstream:  client,
			Registry:  DefaultTools(cfg),
			Guards:    guards,
			Compactor: compactor,
			Config:    cfg.Agent,
			Log:       log,
			Dialect:   tools.SchemaDialect(profile.SchemaDialect),
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
	v1.HandleFunc("POST /v1/sessions", s.handleCreateSession)
	v1.HandleFunc("GET /v1/sessions", s.handleListSessions)
	v1.HandleFunc("GET /v1/sessions/{id}", s.handleGetSession)
	v1.HandleFunc("DELETE /v1/sessions/{id}", s.handleDeleteSession)

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
