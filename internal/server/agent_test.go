package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/oai"
)

// scriptedProvider replays assistant turns in order, so a whole agent run is
// deterministic end to end through the real HTTP handler.
type scriptedProvider struct {
	mu    sync.Mutex
	turns []string // raw JSON for choices[0].message
	n     int
}

func (p *scriptedProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	msg := `{"role":"assistant","content":"out of scripted turns"}`
	if p.n < len(p.turns) {
		msg = p.turns[p.n]
	}
	p.n++
	p.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	payload := `{"id":"chatcmpl-1","object":"chat.completion","created":1700000000,` +
		`"model":"test-model","choices":[{"index":0,"message":` + msg +
		`,"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	w.Write([]byte(payload))
}

func toolTurn(id, name, args string) string {
	return `{"role":"assistant","content":null,"tool_calls":[{"id":"` + id +
		`","type":"function","function":{"name":"` + name +
		`","arguments":` + quote(args) + `}}]}`
}

// quote JSON-encodes an argument string, since tool arguments travel as a
// string field rather than as a nested object.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// newAgentServer builds a server over a workspace seeded with files.
func newAgentServer(t *testing.T, files map[string]string, turns []string) (front string, workspace string) {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	srv := newTestServer(t, (&scriptedProvider{turns: turns}).ServeHTTP,
		func(c *config.Config) {})
	return srv.URL, dir
}

func TestAgentEditsAWorkspaceOverHTTP(t *testing.T) {
	// The whole path: an OpenAI-shaped request arrives, the loop calls tools,
	// and a file on disk actually changes.
	front, dir := newAgentServer(t, map[string]string{
		"main.go": "package main\n\nfunc oldName() {}\n",
	}, []string{
		toolTurn("c1", "read", `{"path":"main.go"}`),
		toolTurn("c2", "edit", `{"path":"main.go","old_string":"oldName","new_string":"newName"}`),
		`{"role":"assistant","content":"Renamed oldName to newName."}`,
	})

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "rename oldName"}},
		"harness":  map[string]any{"workspace": dir},
	})

	resp := postJSON(t, front+"/v1/chat/completions", string(body), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var out oai.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if got := out.Choices[0].Message.Content.String(); !strings.Contains(got, "newName") {
		t.Errorf("answer = %q", got)
	}

	changed, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(changed), "func newName()") {
		t.Errorf("the file on disk was not edited: %q", changed)
	}

	// Usage is the total across all three upstream calls, not the last one's.
	if out.Usage == nil || out.Usage.TotalTokens != 45 {
		t.Errorf("usage = %+v, want 45 total tokens across 3 calls", out.Usage)
	}
	// The session id comes back so a caller can reattach to this workspace.
	if resp.Header.Get("X-Harness-Session") == "" {
		t.Error("no session id was returned")
	}
}

func TestAgentSessionCanBeReattached(t *testing.T) {
	front, dir := newAgentServer(t, map[string]string{"a.txt": "one\n"}, []string{
		toolTurn("c1", "read", `{"path":"a.txt"}`),
		`{"role":"assistant","content":"read it"}`,
		toolTurn("c2", "edit", `{"path":"a.txt","old_string":"one","new_string":"two"}`),
		`{"role":"assistant","content":"edited it"}`,
	})

	first, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "read a.txt"}},
		"harness":  map[string]any{"workspace": dir},
	})
	resp := postJSON(t, front+"/v1/chat/completions", string(first), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d", resp.StatusCode)
	}
	session := resp.Header.Get("X-Harness-Session")
	if session == "" {
		t.Fatal("no session id returned")
	}

	// The second request reuses the session, so the ledger still remembers the
	// read and the edit is authorised without re-reading.
	second, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "now change it"}},
		"harness":  map[string]any{"session_id": session},
	})
	resp2 := postJSON(t, front+"/v1/chat/completions", string(second), nil)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d", resp2.StatusCode)
	}

	changed, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(changed) != "two\n" {
		t.Errorf("content = %q — the session did not carry the observation across requests", changed)
	}
}

func TestAgentUnknownSessionIsRejected(t *testing.T) {
	front, _ := newAgentServer(t, nil, []string{`{"role":"assistant","content":"hi"}`})

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"harness":  map[string]any{"session_id": "ws_00000000000000000000000000000000"},
	})
	resp := postJSON(t, front+"/v1/chat/completions", string(body), nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an expired session", resp.StatusCode)
	}
	var env struct {
		Error struct{ Message string } `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&env)
	if !strings.Contains(env.Error.Message, "session") {
		t.Errorf("error does not mention the session: %q", env.Error.Message)
	}
}

func TestAgentToolErrorDoesNotFailTheRequest(t *testing.T) {
	// A tool failure is a normal turn. It must not surface as an HTTP error.
	front, _ := newAgentServer(t, nil, []string{
		toolTurn("c1", "read", `{"path":"does-not-exist.go"}`),
		`{"role":"assistant","content":"That file is not there."}`,
	})

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "read it"}},
	})
	resp := postJSON(t, front+"/v1/chat/completions", string(body), nil)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a tool error is not a transport error", resp.StatusCode)
	}
}

func TestAgentWorkspaceOutsideIsRejectedCleanly(t *testing.T) {
	front, _ := newAgentServer(t, nil, []string{`{"role":"assistant","content":"hi"}`})

	body, _ := json.Marshal(map[string]any{
		"model":    "test-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"harness":  map[string]any{"workspace": "/nonexistent/path/for/this/test"},
	})
	resp := postJSON(t, front+"/v1/chat/completions", string(body), nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
