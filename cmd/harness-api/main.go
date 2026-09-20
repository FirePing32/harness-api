// Command harness-api serves an agentic coding loop behind an OpenAI-compatible API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/logx"
	"github.com/FirePing32/harness-api/internal/server"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "harness-api: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("harness-api", flag.ContinueOnError)

	var (
		configPath       string
		addr             string
		allowNonLoopback bool
		baseURL          string
		model            string
		profile          string
		workspaceRoot    string
		logLevel         string
		logFormat        string
		auditPath        string
	)

	fs.StringVar(&configPath, "config", os.Getenv("HARNESS_CONFIG"), "path to a JSON config file")
	fs.StringVar(&addr, "addr", "", "listen address (default 127.0.0.1:8080)")
	fs.BoolVar(&allowNonLoopback, "allow-non-loopback", false,
		"permit binding a non-loopback address; this exposes shell execution to the network")
	fs.StringVar(&baseURL, "upstream-base-url", "", "OpenAI-compatible base URL")
	fs.StringVar(&model, "upstream-model", "", "default upstream model")
	fs.StringVar(&profile, "upstream-profile", "", "provider quirk profile name")
	fs.StringVar(&workspaceRoot, "workspace-root", "", "parent directory for ephemeral workspaces")
	fs.StringVar(&auditPath, "audit-log", "",
		"append a JSON-lines record of every command and refusal to this file")
	fs.StringVar(&logLevel, "log-level", "", "debug|info|warn|error")
	fs.StringVar(&logFormat, "log-format", "", "json|text")

	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := config.Default()
	if configPath != "" {
		if err := cfg.LoadFile(configPath); err != nil {
			return err
		}
	}
	if err := cfg.ApplyEnv(); err != nil {
		return err
	}

	// Only flags the operator actually passed may override file and env. Reading
	// the variables unconditionally would let an unset flag's zero value win.
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "addr":
			cfg.Server.Addr = addr
		case "allow-non-loopback":
			cfg.Server.AllowNonLoopback = allowNonLoopback
		case "upstream-base-url":
			cfg.Upstream.BaseURL = baseURL
		case "upstream-model":
			cfg.Upstream.Model = model
		case "upstream-profile":
			cfg.Upstream.Profile = profile
		case "workspace-root":
			cfg.Workspace.Root = workspaceRoot
		case "audit-log":
			cfg.Audit.Path = auditPath
		case "log-level":
			cfg.Log.Level = logLevel
		case "log-format":
			cfg.Log.Format = logFormat
		}
	})

	if err := cfg.Validate(); err != nil {
		return err
	}

	log := logx.New(logx.Options{Level: cfg.Log.Level, Format: cfg.Log.Format})

	if loopback, err := config.IsLoopback(cfg.Server.Addr); err == nil && !loopback {
		log.Warn("listening on a non-loopback address; callers of this API can execute "+
			"arbitrary shell commands as this process's user",
			"addr", cfg.Server.Addr)
	}
	if len(cfg.Server.AuthTokens) == 0 {
		log.Warn("no auth tokens configured; any local process can drive this agent")
	}
	if cfg.Shell.Enabled {
		log.Warn("the bash tool is enabled; callers of this API can run arbitrary "+
			"commands as this process's user, and those commands are not confined "+
			"to the workspace",
			"disable_with", "shell.enabled=false or HARNESS_SHELL_ENABLED=false")
	}

	srv, err := server.New(&cfg, log)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Idle workspaces are reclaimed in the background. The sweeper is tied to
	// the signal context so it stops before the shutdown path starts removing
	// the same directories.
	go srv.Sessions().Run(ctx)

	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Duration(),
		ErrorLog:          nil,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("harness-api listening",
			"addr", cfg.Server.Addr,
			"upstream", cfg.Upstream.BaseURL,
			"profile", cfg.Upstream.Profile,
			"workspace_root", cfg.Workspace.Root)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		stop() // restore default handling, so a second signal kills immediately
		log.Info("shutdown signal received", "grace", cfg.Server.ShutdownGrace.String())
	}

	shutdownCtx, cancel := context.WithTimeout(
		context.Background(), cfg.Server.ShutdownGrace.Duration())
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	// After in-flight requests have drained, so nothing has a workspace pulled
	// out from under it.
	if err := srv.Sessions().Close(); err != nil {
		log.Error("releasing workspaces", "error", err)
	}

	// Last, so that anything the drain above audited is on disk.
	if err := srv.Audit().Close(); err != nil {
		log.Error("closing the audit log", "error", err)
	}

	log.Info("shutdown complete")
	return nil
}
