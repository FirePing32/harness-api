// Package upstream talks to whichever OpenAI-compatible provider backs the agent.
//
// This package owns the only code path that serialises a request for a provider.
// That is load-bearing: inbound-only fields are stripped exactly here, so "never
// send X upstream" is enforced in one place rather than relied upon at every call
// site.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/prakhargurunani/harness-api/internal/config"
	"github.com/prakhargurunani/harness-api/internal/logx"
	"github.com/prakhargurunani/harness-api/internal/oai"
)

// maxErrorBodyBytes caps how much of a failed response we read. Some providers
// answer errors with very large HTML pages, and none of it is useful.
const maxErrorBodyBytes = 8 << 10

// Client is a provider client. It is safe for concurrent use.
type Client struct {
	baseURL    string
	apiKey     string
	model      string
	maxRetries int
	http       *http.Client
	log        *slog.Logger
}

// New builds a Client from upstream configuration.
func New(cfg config.Upstream, log *slog.Logger) *Client {
	return &Client{
		baseURL:    cfg.BaseURL,
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		maxRetries: cfg.MaxRetries,
		http: &http.Client{
			Timeout: cfg.Timeout.Duration(),
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 64,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		log: log,
	}
}

// DefaultModel is the configured fallback model, used when a request does not
// name one.
func (c *Client) DefaultModel() string { return c.model }

// BaseURL is the configured provider endpoint.
func (c *Client) BaseURL() string { return c.baseURL }

// Complete performs a non-streaming chat completion.
func (c *Client) Complete(ctx context.Context, req *oai.ChatCompletionRequest) (*oai.ChatCompletionResponse, error) {
	body, err := BuildBody(req, false)
	if err != nil {
		return nil, fmt.Errorf("encode upstream request: %w", err)
	}

	resp, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}

	var out oai.ChatCompletionResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		// A 200 with an unparseable body happens with misconfigured proxies that
		// return an HTML login page. Say which it was rather than "invalid character".
		return nil, fmt.Errorf("upstream returned a non-JSON success body (%d bytes): %w",
			len(raw), err)
	}
	return &out, nil
}

// BuildBody serialises a request for a provider. This is the chokepoint where
// inbound-only fields are removed:
//
//   - harness: this server's own extension object, meaningless to a provider.
//   - reasoning_content: chain-of-thought echoed back in history. DeepSeek returns
//     400 if it sees its own reasoning in a subsequent request, and no provider
//     needs it, so it never goes out.
//
// The input request is not modified; callers keep the full-fidelity history.
func BuildBody(req *oai.ChatCompletionRequest, stream bool) ([]byte, error) {
	out := *req
	out.Harness = nil
	out.Stream = stream
	if !stream {
		out.StreamOptions = nil
	}

	out.Messages = make([]oai.Message, len(req.Messages))
	for i, m := range req.Messages {
		m.ReasoningContent = ""
		// Some providers deliver reasoning under a different key, which our
		// unknown-field capture would faithfully echo back at them.
		if len(m.Extra) > 0 {
			cleaned := make(map[string]json.RawMessage, len(m.Extra))
			for k, v := range m.Extra {
				if _, banned := bannedMessageKeys[k]; banned {
					continue
				}
				cleaned[k] = v
			}
			m.Extra = cleaned
		}
		out.Messages[i] = m
	}

	if len(out.Extra) > 0 {
		cleaned := make(map[string]json.RawMessage, len(out.Extra))
		for k, v := range out.Extra {
			if k == "harness" {
				continue
			}
			cleaned[k] = v
		}
		out.Extra = cleaned
	}

	return json.Marshal(out)
}

// bannedMessageKeys are message members that must never be echoed to a provider.
var bannedMessageKeys = map[string]struct{}{
	"reasoning_content": {},
	"reasoning":         {},
	"thinking":          {},
	"harness":           {},
}

// do issues the HTTP request with retries. The caller owns closing the body.
func (c *Client) do(ctx context.Context, body []byte) (*http.Response, error) {
	url := c.baseURL + "/chat/completions"

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt, lastErr)
			c.log.Debug("retrying upstream request",
				"attempt", attempt, "delay", delay, "reason", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build upstream request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			// A cancelled context is the client going away, not a flaky provider.
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("upstream request failed: %w", err)
			continue
		}

		if resp.StatusCode < 400 {
			return resp, nil
		}

		upErr := readUpstreamError(resp)
		resp.Body.Close()

		if !upErr.Retryable() {
			return nil, upErr
		}
		lastErr = upErr
	}

	return nil, fmt.Errorf("upstream failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

// Error is a non-2xx response from the provider.
type Error struct {
	Status     int
	Body       string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.Status, e.Body)
}

// Retryable reports whether trying again could plausibly succeed. 408, 409, 425,
// 429 and 5xx qualify; a 400 means the request itself is wrong and will be wrong
// again.
func (e *Error) Retryable() bool {
	switch e.Status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return e.Status >= 500
}

func readUpstreamError(resp *http.Response) *Error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return &Error{
		Status:     resp.StatusCode,
		Body:       logx.Redact(string(bytes.TrimSpace(body))),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

// parseRetryAfter handles both forms the header takes: a delay in seconds, and an
// absolute HTTP date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoff returns how long to wait before a retry. A provider-supplied Retry-After
// always wins; otherwise exponential with full jitter, which avoids a thundering
// herd when many sessions hit the same rate limit at once.
func backoff(attempt int, lastErr error) time.Duration {
	var upErr *Error
	if errors.As(lastErr, &upErr) && upErr.RetryAfter > 0 {
		return min(upErr.RetryAfter, 60*time.Second)
	}
	base := time.Duration(1<<uint(attempt-1)) * 500 * time.Millisecond
	base = min(base, 30*time.Second)
	return base/2 + time.Duration(rand.Int64N(int64(base/2)+1))
}

// ToAPIError maps a provider failure onto a client-facing error.
//
// The mapping is not a passthrough, because upstream status codes mean different
// things once relayed. A 401 from the provider means this server's credentials are
// wrong; returning 401 to the caller would tell them to fix their own token, which
// is the opposite of the truth.
func ToAPIError(err error) *oai.APIError {
	var upErr *Error
	if !errors.As(err, &upErr) {
		return oai.NewAPIError("upstream request failed", err)
	}

	switch {
	case upErr.Status == http.StatusUnauthorized || upErr.Status == http.StatusForbidden:
		e := oai.NewAPIError(
			"the upstream provider rejected this server's credentials; "+
				"check the server's upstream.api_key configuration", err)
		e.Status = http.StatusBadGateway
		return e

	case upErr.Status == http.StatusTooManyRequests:
		return oai.NewRateLimit("the upstream provider is rate limiting this server: " +
			truncate(upErr.Body, 300))

	case upErr.Status >= 400 && upErr.Status < 500:
		// The caller's own request is most likely at fault here (unknown model,
		// unsupported parameter), so the provider's message is genuinely useful.
		// It has already been credential-redacted by readUpstreamError.
		e := oai.NewInvalidRequest(
			fmt.Sprintf("upstream rejected the request (%d): %s",
				upErr.Status, truncate(upErr.Body, 500)), "")
		return e

	default:
		e := oai.NewAPIError("the upstream provider is unavailable", err)
		e.Status = http.StatusBadGateway
		return e
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
