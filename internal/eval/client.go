package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/FirePing32/harness-api/internal/agent"
	"github.com/FirePing32/harness-api/internal/oai"
	"github.com/FirePing32/harness-api/internal/upstream"
)

// The eval talks to a running server as an ordinary OpenAI client, over HTTP,
// with no privileged access to anything inside it.
//
// It could call agent.Loop directly and save a process boundary. That would
// also stop measuring the server: binding resolution, the quirk transforms,
// the guard chain, streaming assembly. Those are where several of the
// interesting failures live, and a harness that cannot see them is measuring a
// component rather than the product.
//
// Streaming is used even though the eval has no interest in progressive
// output, because the opt-in progress events are the only way to see what
// happened inside a run — which tools were called, which failed, and with what
// code. Those are three of the four metrics the suite exists to produce.

// Transcript is everything one request revealed.
type Transcript struct {
	Answer string
	Events []agent.Event

	SessionID string
	Usage     oai.Usage
	Finish    string
	Stop      agent.StopReason

	// Turns is the number of model turns, counted from the events rather than
	// taken from the done event, so a stream that ends early still reports
	// what it got through.
	Turns int
}

// ToolStats is per-tool call and error counts.
type ToolStats map[string]*ToolStat

// ToolStat counts calls and failures for one tool.
type ToolStat struct {
	Calls  int            `json:"calls"`
	Errors int            `json:"errors"`
	Codes  map[string]int `json:"codes,omitempty"`
}

// Tools aggregates the transcript's tool activity.
//
// The per-tool error rate is the direct feedback signal on tool ergonomics. If
// edit returns an error on a third of its calls, the fix is in the error
// messages edit produces, not in the system prompt — and without this number
// there is no way to tell those two hypotheses apart.
func (t *Transcript) Tools() ToolStats {
	stats := ToolStats{}
	for _, ev := range t.Events {
		if ev.Type != agent.EventToolEnd || ev.Tool == "" {
			continue
		}
		s := stats[ev.Tool]
		if s == nil {
			s = &ToolStat{Codes: map[string]int{}}
			stats[ev.Tool] = s
		}
		s.Calls++
		if ev.IsError {
			s.Errors++
			code := ev.Code
			if code == "" {
				code = "unknown"
			}
			s.Codes[code]++
		}
	}
	return stats
}

// Client speaks to a harness-api server.
type Client struct {
	BaseURL string
	APIKey  string
	Model   string
	HTTP    *http.Client
}

// NewClient builds a Client. The timeout is per request and generous, because
// an agent run legitimately takes minutes; the per-task context is what
// actually bounds a run.
func NewClient(baseURL, apiKey, model string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 30 * time.Minute},
	}
}

// Request is one agent invocation.
type Request struct {
	Prompt    string
	Workspace string
	Tools     []string
}

// Run sends the request and consumes the whole stream.
func (c *Client) Run(ctx context.Context, req Request) (*Transcript, error) {
	body, err := json.Marshal(&oai.ChatCompletionRequest{
		Model:    c.Model,
		Messages: []oai.Message{{Role: oai.RoleUser, Content: oai.TextContent(req.Prompt)}},
		Stream:   true,
		Harness: &oai.HarnessRequestExt{
			Workspace:    req.Workspace,
			StreamEvents: true,
			Tools:        req.Tools,
		},
	})
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, statusError(resp)
	}

	return c.consume(resp)
}

// statusError renders a non-200 usefully.
//
// The body is included here, unlike anywhere else in this project, because the
// server being talked to is the one under test and the person reading the
// message is its operator. There is no third party whose credentials could be
// reflected back.
func statusError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	text := strings.TrimSpace(string(raw))

	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Error.Message != "" {
		text = env.Error.Message
	}
	return fmt.Errorf("server returned %s: %s", resp.Status, text)
}

func (c *Client) consume(resp *http.Response) (*Transcript, error) {
	// The same decoder the server uses against providers. Reusing it means the
	// eval exercises the parser rather than a second implementation that could
	// agree with the server while both are wrong.
	stream := upstream.NewStream(resp.Body, upstream.FormatSSE)
	defer stream.Close()

	t := &Transcript{SessionID: resp.Header.Get("X-Harness-Session")}
	var answer strings.Builder

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading stream: %w", err)
		}
		if chunk.Usage != nil {
			t.Usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		if choice.Delta.Content != nil {
			answer.WriteString(*choice.Delta.Content)
		}
		if choice.FinishReason != nil {
			t.Finish = *choice.FinishReason
		}
		if len(choice.Delta.Harness) > 0 {
			var ev agent.Event
			if json.Unmarshal(choice.Delta.Harness, &ev) != nil {
				continue
			}
			t.Events = append(t.Events, ev)
			switch ev.Type {
			case agent.EventTurnStart:
				t.Turns++
			case agent.EventDone:
				t.Stop = ev.Stop
			}
		}
	}

	t.Answer = answer.String()
	return t, nil
}
