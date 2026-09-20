package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsLoopback(t *testing.T) {
	tests := []struct {
		addr         string
		want         bool
		wantErr      bool
		whyItMatters string
	}{
		{addr: "127.0.0.1:8080", want: true},
		{addr: "localhost:8080", want: true},
		{addr: "[::1]:8080", want: true},
		{addr: ":8080", want: false, whyItMatters: "empty host binds every interface"},
		{addr: "0.0.0.0:8080", want: false},
		{addr: "192.168.1.5:8080", want: false},
		{addr: "example.com:8080", want: false, whyItMatters: "unresolvable names must fail closed"},
		{addr: "not-an-address", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got, err := IsLoopback(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("IsLoopback(%q) = %v, want error", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("IsLoopback(%q): unexpected error: %v", tt.addr, err)
			}
			if got != tt.want {
				t.Errorf("IsLoopback(%q) = %v, want %v (%s)", tt.addr, got, tt.want, tt.whyItMatters)
			}
		})
	}
}

func TestValidateRejectsUnguardedNonLoopback(t *testing.T) {
	cfg := Default()
	cfg.Server.Addr = "0.0.0.0:8080"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected Validate to reject a non-loopback bind without allow_non_loopback")
	}
	if !strings.Contains(err.Error(), "allow_non_loopback") {
		t.Errorf("error should name the flag that unblocks it, got: %v", err)
	}
}

func TestValidateRequiresAuthWhenExposed(t *testing.T) {
	cfg := Default()
	cfg.Server.Addr = "0.0.0.0:8080"
	cfg.Server.AllowNonLoopback = true

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected Validate to require auth tokens on a non-loopback bind")
	}
	if !strings.Contains(err.Error(), "auth_tokens") {
		t.Errorf("error should name auth_tokens, got: %v", err)
	}

	cfg.Server.AuthTokens = []string{"secret"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config with auth should validate: %v", err)
	}
}

func TestValidateFillsWorkspaceRoot(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Workspace.Root == "" {
		t.Fatal("Validate should derive a workspace root when none is configured")
	}
}

func TestValidateTrimsTrailingSlashFromBaseURL(t *testing.T) {
	cfg := Default()
	cfg.Upstream.BaseURL = "https://api.example.com/v1/"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Request paths are joined with "/chat/completions"; a trailing slash here
	// produces a double slash that some gateways route differently.
	if cfg.Upstream.BaseURL != "https://api.example.com/v1" {
		t.Errorf("base URL = %q, want trailing slash trimmed", cfg.Upstream.BaseURL)
	}
}

func TestLoadFileRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"server":{"addrr":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Default()
	err := cfg.LoadFile(path)
	if err == nil {
		t.Fatal("a misspelled config key should be an error, not silently ignored")
	}
}

func TestDurationAcceptsStringAndNanos(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"server":{"shutdown_grace":"45s"},"workspace":{"idle_ttl":60000000000}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Default()
	if err := cfg.LoadFile(path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if got := cfg.Server.ShutdownGrace.Duration(); got != 45*time.Second {
		t.Errorf("shutdown_grace = %v, want 45s", got)
	}
	if got := cfg.Workspace.IdleTTL.Duration(); got != time.Minute {
		t.Errorf("idle_ttl = %v, want 1m", got)
	}
}

func TestApplyEnvOverridesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"upstream":{"model":"from-file"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HARNESS_UPSTREAM_MODEL", "from-env")

	cfg := Default()
	if err := cfg.LoadFile(path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if err := cfg.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Upstream.Model != "from-env" {
		t.Errorf("model = %q, want env to win over file", cfg.Upstream.Model)
	}
}

func TestApplyEnvFallsBackToOpenAIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-fallback")

	cfg := Default()
	if err := cfg.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Upstream.APIKey != "sk-fallback" {
		t.Errorf("APIKey = %q, want OPENAI_API_KEY used as fallback", cfg.Upstream.APIKey)
	}

	// The harness-specific variable must take precedence over the generic one.
	t.Setenv("HARNESS_UPSTREAM_API_KEY", "sk-specific")
	cfg = Default()
	if err := cfg.ApplyEnv(); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Upstream.APIKey != "sk-specific" {
		t.Errorf("APIKey = %q, want HARNESS_UPSTREAM_API_KEY to win", cfg.Upstream.APIKey)
	}
}

func TestApplyEnvReportsBadValues(t *testing.T) {
	t.Setenv("HARNESS_MAX_WALL_CLOCK", "not-a-duration")

	cfg := Default()
	if err := cfg.ApplyEnv(); err == nil {
		t.Fatal("an unparseable duration should be a startup error")
	}
}

func TestSplitAndTrim(t *testing.T) {
	got := splitAndTrim(" a , b ,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitAndTrim = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitAndTrim = %v, want %v", got, want)
		}
	}
}
