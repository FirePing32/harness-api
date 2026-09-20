package logx

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestIsSensitiveCatchesCredentialKeys(t *testing.T) {
	for _, key := range []string{
		"authorization", "Authorization",
		"api_key", "apiKey", "apikey", "x-api-key", "upstream_api_key",
		"token", "tokens",
		"access_token", "auth_token", "refresh_token", "id_token",
		"api_token", "session_token", "bearer_token", "oauth_token",
		"secret", "client_secret", "password", "passwd",
		"credential", "aws_credentials",
		"request.headers.authorization",
	} {
		if !isSensitive(key) {
			t.Errorf("isSensitive(%q) = false; this key holds a credential", key)
		}
	}
}

func TestIsSensitiveLeavesUsageMetricsReadable(t *testing.T) {
	// The reason segment matching exists. A substring test against "token"
	// redacts every one of these, and they are the numbers that reveal a
	// runaway agent loop — the failure is silent and looks like a working log.
	for _, key := range []string{
		"total_tokens", "prompt_tokens", "completion_tokens",
		"cached_tokens", "reasoning_tokens", "max_tokens", "token_count",
		"tokens_per_second",
		"keyboard", "monkey", "passwords_checked_count",
	} {
		if isSensitive(key) {
			t.Errorf("isSensitive(%q) = true; this is a metric, not a credential", key)
		}
	}
}

func TestIsSensitiveOnUnrelatedKeys(t *testing.T) {
	for _, key := range []string{
		"session_id", "request_id", "duration_ms", "status", "path",
		"tool", "turns", "stop", "model", "workspace",
	} {
		if isSensitive(key) {
			t.Errorf("isSensitive(%q) = true", key)
		}
	}
}

func TestHandlerRedactsSensitiveAttributes(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Format: "text", Writer: &buf})

	log.Info("outbound", "authorization", "Bearer sk-live-abcdef123456", "total_tokens", 480)

	out := buf.String()
	if strings.Contains(out, "sk-live-abcdef123456") {
		t.Errorf("the credential survived: %s", out)
	}
	if !strings.Contains(out, redacted) {
		t.Errorf("nothing was redacted: %s", out)
	}
	if !strings.Contains(out, "total_tokens=480") {
		t.Errorf("the usage metric was lost: %s", out)
	}
}

func TestHandlerRedactsCredentialsInFreeText(t *testing.T) {
	// Several providers echo the request they received, Authorization header
	// and all, inside an error body.
	var buf bytes.Buffer
	log := New(Options{Format: "text", Writer: &buf})

	log.Error("upstream rejected the request",
		"body", `{"error":{"message":"bad request, received Authorization: Bearer sk-live-abcdef123456"}}`)

	if out := buf.String(); strings.Contains(out, "sk-live-abcdef123456") {
		t.Errorf("a credential in an error body survived: %s", out)
	}
}

func TestHandlerRedactsTheMessageItself(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Format: "text", Writer: &buf})

	log.Info("calling with sk-live-abcdef123456 attached")

	if out := buf.String(); strings.Contains(out, "sk-live-abcdef123456") {
		t.Errorf("a credential in the message survived: %s", out)
	}
}

func TestHandlerRedactsInsideGroupsAndWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := New(Options{Format: "text", Writer: &buf}).
		With("api_key", "sk-live-abcdef123456")

	log.Info("hello", slog.Group("upstream", "token", "sk-live-zzzzzzzz9999", "total_tokens", 12))

	out := buf.String()
	for _, secret := range []string{"sk-live-abcdef123456", "sk-live-zzzzzzzz9999"} {
		if strings.Contains(out, secret) {
			t.Errorf("credential %q survived: %s", secret, out)
		}
	}
	if !strings.Contains(out, "total_tokens=12") {
		t.Errorf("a metric inside a group was lost: %s", out)
	}
}

func TestRedactLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"read 2000 lines from main.go",
		"total_tokens=480 prompt_tokens=400",
		"the tokenizer produced 12 tokens",
	} {
		if got := Redact(s); got != s {
			t.Errorf("Redact(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestParseLevelFallsBackToInfo(t *testing.T) {
	// Bad logging config must never be why a server refuses to boot.
	if got := parseLevel("nonsense"); got != slog.LevelInfo {
		t.Errorf("parseLevel(nonsense) = %v, want info", got)
	}
	if got := parseLevel("DEBUG"); got != slog.LevelDebug {
		t.Errorf("parseLevel is case-sensitive: %v", got)
	}
}
