// Package config holds server configuration and its layered loading rules.
//
// Precedence, lowest to highest: built-in defaults, JSON config file, environment
// variables, explicitly-passed command line flags. "Explicitly passed" matters —
// a flag left at its zero value must not clobber a value set by file or env, so
// flag application is driven by flag.Visit rather than by reading flag values.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration wraps time.Duration so config files can say "30s" instead of 30000000000.
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalJSON(b []byte) error {
	// Accept both "30s" and a raw nanosecond count, since hand-written configs use
	// the former and machine-generated ones tend to use the latter.
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}

	var n int64
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or a nanosecond count")
	}
	*d = Duration(n)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Config is the whole server configuration.
type Config struct {
	Server    Server    `json:"server"`
	Upstream  Upstream  `json:"upstream"`
	Agent     Agent     `json:"agent"`
	Workspace Workspace `json:"workspace"`
	Log       Log       `json:"log"`
}

// Server covers the HTTP listener and its front door.
type Server struct {
	Addr string `json:"addr"`

	// AllowNonLoopback must be set explicitly to bind anywhere other than
	// localhost. This server executes shell commands on behalf of callers; binding
	// it to a routable interface is a deliberate act, not a default.
	AllowNonLoopback bool `json:"allow_non_loopback"`

	// AuthTokens are accepted bearer tokens. Required unless the listener is
	// loopback-only.
	AuthTokens []string `json:"auth_tokens"`

	ReadHeaderTimeout Duration `json:"read_header_timeout"`
	ShutdownGrace     Duration `json:"shutdown_grace"`
	MaxBodyBytes      int64    `json:"max_body_bytes"`
}

// Upstream points at whichever OpenAI-compatible endpoint backs the agent.
type Upstream struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`

	// Model is the default upstream model. A request may override it, which is why
	// this is a default rather than a hard binding.
	Model string `json:"model"`

	// Profile names the quirk profile (see internal/config/profiles.go). Empty
	// means "generic", the maximally conservative setting.
	Profile string `json:"profile"`

	Timeout    Duration `json:"timeout"`
	MaxRetries int      `json:"max_retries"`
}

// Agent bounds a single request's agent loop. Every field here is a ceiling: an
// agent loop that misbehaves costs real money, so each bound has a finite default.
type Agent struct {
	MaxIterations    int      `json:"max_iterations"`
	MaxWallClock     Duration `json:"max_wall_clock"`
	MaxTotalTokens   int      `json:"max_total_tokens"`
	MaxParallelTools int      `json:"max_parallel_tools"`
}

// Workspace governs where agent sessions do their work.
type Workspace struct {
	// Root is the parent directory for ephemeral session workspaces.
	Root string `json:"root"`

	// Default, if set, is used when a request does not name a workspace. Empty
	// means every unbound request gets a fresh ephemeral directory.
	Default string `json:"default"`

	IdleTTL Duration `json:"idle_ttl"`
}

// Log configures the root logger.
type Log struct {
	Level  string `json:"level"`
	Format string `json:"format"`
}

// Default returns the built-in configuration, before file, env, or flags.
func Default() Config {
	return Config{
		Server: Server{
			Addr:              "127.0.0.1:8080",
			ReadHeaderTimeout: Duration(10 * time.Second),
			ShutdownGrace:     Duration(20 * time.Second),
			MaxBodyBytes:      1 << 20, // 1 MiB
		},
		Upstream: Upstream{
			BaseURL:    "https://api.openai.com/v1",
			Profile:    "generic",
			Timeout:    Duration(10 * time.Minute),
			MaxRetries: 3,
		},
		Agent: Agent{
			MaxIterations:    50,
			MaxWallClock:     Duration(10 * time.Minute),
			MaxTotalTokens:   1_000_000,
			MaxParallelTools: 10,
		},
		Workspace: Workspace{
			Root:    "", // resolved to os.TempDir()/harness-api at Validate
			IdleTTL: Duration(30 * time.Minute),
		},
		Log: Log{Level: "info", Format: "text"},
	}
}

// LoadFile layers a JSON config file onto c. A missing path is an error; callers
// that treat the file as optional should check for existence first.
func (c *Config) LoadFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // a typo'd key should fail loudly, not be ignored
	if err := dec.Decode(c); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

// ApplyEnv layers HARNESS_* environment variables onto c.
func (c *Config) ApplyEnv() error {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	num := func(key string, dst *int) error {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*dst = n
		return nil
	}
	dur := func(key string, dst *Duration) error {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*dst = Duration(d)
		return nil
	}

	str("HARNESS_ADDR", &c.Server.Addr)
	if v, ok := os.LookupEnv("HARNESS_AUTH_TOKENS"); ok {
		c.Server.AuthTokens = splitAndTrim(v)
	}
	if v, ok := os.LookupEnv("HARNESS_ALLOW_NON_LOOPBACK"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("HARNESS_ALLOW_NON_LOOPBACK: %w", err)
		}
		c.Server.AllowNonLoopback = b
	}

	str("HARNESS_UPSTREAM_BASE_URL", &c.Upstream.BaseURL)
	str("HARNESS_UPSTREAM_API_KEY", &c.Upstream.APIKey)
	str("HARNESS_UPSTREAM_MODEL", &c.Upstream.Model)
	str("HARNESS_UPSTREAM_PROFILE", &c.Upstream.Profile)
	if err := dur("HARNESS_UPSTREAM_TIMEOUT", &c.Upstream.Timeout); err != nil {
		return err
	}

	// OPENAI_* is honoured as a convenience so an existing shell environment works
	// without translation, but HARNESS_* wins where both are present.
	if c.Upstream.APIKey == "" {
		str("OPENAI_API_KEY", &c.Upstream.APIKey)
	}

	if err := num("HARNESS_MAX_ITERATIONS", &c.Agent.MaxIterations); err != nil {
		return err
	}
	if err := dur("HARNESS_MAX_WALL_CLOCK", &c.Agent.MaxWallClock); err != nil {
		return err
	}

	str("HARNESS_WORKSPACE_ROOT", &c.Workspace.Root)
	str("HARNESS_WORKSPACE_DEFAULT", &c.Workspace.Default)
	if err := dur("HARNESS_WORKSPACE_IDLE_TTL", &c.Workspace.IdleTTL); err != nil {
		return err
	}

	str("HARNESS_LOG_LEVEL", &c.Log.Level)
	str("HARNESS_LOG_FORMAT", &c.Log.Format)
	return nil
}

// Validate normalises derived values and rejects configurations that are unsafe or
// cannot work. It mutates c, so call it last.
func (c *Config) Validate() error {
	var errs []error

	if c.Server.Addr == "" {
		errs = append(errs, errors.New("server.addr is empty"))
	}

	loopback, err := IsLoopback(c.Server.Addr)
	if err != nil {
		errs = append(errs, fmt.Errorf("server.addr: %w", err))
	} else if !loopback && !c.Server.AllowNonLoopback {
		errs = append(errs, fmt.Errorf(
			"refusing to bind %q: this server executes shell commands for its callers, "+
				"so binding a non-loopback address requires server.allow_non_loopback "+
				"(or --allow-non-loopback) to be set explicitly", c.Server.Addr))
	}

	// A routable listener without auth is an open remote shell. Never allow it.
	if !loopback && len(c.Server.AuthTokens) == 0 {
		errs = append(errs, errors.New(
			"server.auth_tokens must be set when binding a non-loopback address"))
	}

	if c.Upstream.BaseURL == "" {
		errs = append(errs, errors.New("upstream.base_url is empty"))
	}
	c.Upstream.BaseURL = strings.TrimRight(c.Upstream.BaseURL, "/")

	if c.Upstream.Profile == "" {
		c.Upstream.Profile = "generic"
	}
	if c.Upstream.MaxRetries < 0 {
		errs = append(errs, errors.New("upstream.max_retries must be >= 0"))
	}

	if c.Agent.MaxIterations <= 0 {
		errs = append(errs, errors.New("agent.max_iterations must be > 0"))
	}
	if c.Agent.MaxParallelTools <= 0 {
		errs = append(errs, errors.New("agent.max_parallel_tools must be > 0"))
	}
	if c.Agent.MaxWallClock <= 0 {
		errs = append(errs, errors.New("agent.max_wall_clock must be > 0"))
	}

	if c.Workspace.Root == "" {
		c.Workspace.Root = defaultWorkspaceRoot()
	}
	if c.Workspace.IdleTTL <= 0 {
		errs = append(errs, errors.New("workspace.idle_ttl must be > 0"))
	}

	if c.Server.MaxBodyBytes <= 0 {
		errs = append(errs, errors.New("server.max_body_bytes must be > 0"))
	}

	return errors.Join(errs...)
}

func defaultWorkspaceRoot() string {
	return strings.TrimRight(os.TempDir(), "/") + "/harness-api"
}

// IsLoopback reports whether a listen address binds only the loopback interface.
// An empty or wildcard host binds every interface and is therefore not loopback —
// the case most likely to be assumed safe by mistake.
func IsLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("not a host:port address: %w", err)
	}
	switch host {
	case "":
		return false, nil // ":8080" binds every interface
	case "localhost":
		return true, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A hostname we cannot classify without resolving. Treat as non-loopback:
		// the failure mode of guessing wrong in the other direction is an open shell.
		return false, nil
	}
	return ip.IsLoopback(), nil
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
