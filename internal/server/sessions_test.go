package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func doJSON(t *testing.T, method, url, body string) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out map[string]any
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unparseable body from %s %s: %s", method, url, raw)
		}
	}
	return resp, out
}

func TestSessionCreateAndDelete(t *testing.T) {
	front, _ := newAgentServer(t, nil, nil)

	resp, body := doJSON(t, http.MethodPost, front+"/v1/sessions", "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %v", resp.StatusCode, body)
	}

	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("no session id returned")
	}
	if resp.Header.Get(HeaderSession) != id {
		t.Error("the session id is not in the response header")
	}
	if body["ephemeral"] != true {
		t.Error("a session with no workspace should own its directory")
	}

	dir, _ := body["workspace"].(string)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the workspace directory was not created: %v", err)
	}

	resp, body = doJSON(t, http.MethodDelete, front+"/v1/sessions/"+id, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d: %v", resp.StatusCode, body)
	}
	if body["workspace_removed"] != true {
		t.Error("deleting an ephemeral session should remove its directory")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the ephemeral directory survived deletion")
	}
}

func TestSessionDeleteLeavesACallerDirectoryAlone(t *testing.T) {
	// The destructive case. A session bound to somebody's working tree gives up
	// the handle and nothing else.
	front, _ := newAgentServer(t, nil, nil)

	project := t.TempDir()
	keep := filepath.Join(project, "important.go")
	if err := os.WriteFile(keep, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, body := doJSON(t, http.MethodPost, front+"/v1/sessions",
		`{"workspace":"`+project+`"}`)
	id, _ := body["id"].(string)
	if body["ephemeral"] != false {
		t.Error("a caller-supplied workspace must not be marked ephemeral")
	}

	_, body = doJSON(t, http.MethodDelete, front+"/v1/sessions/"+id, "")
	if body["workspace_removed"] != false {
		t.Error("workspace_removed should be false for a caller's own directory")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("deleting the session destroyed the caller's files: %v", err)
	}
}

func TestSessionGetAndList(t *testing.T) {
	front, _ := newAgentServer(t, nil, nil)

	_, created := doJSON(t, http.MethodPost, front+"/v1/sessions", "")
	id, _ := created["id"].(string)

	resp, body := doJSON(t, http.MethodGet, front+"/v1/sessions/"+id, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if body["id"] != id {
		t.Errorf("id = %v, want %q", body["id"], id)
	}
	// The expiry has to be visible, or a caller cannot tell when the workspace
	// is about to be reclaimed underneath them.
	if body["expires_at"] == nil {
		t.Error("no expiry reported")
	}

	resp, list := doJSON(t, http.MethodGet, front+"/v1/sessions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	data, _ := list["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("listed %d sessions, want 1", len(data))
	}
}

func TestSessionUnknownIDIsNotFound(t *testing.T) {
	front, _ := newAgentServer(t, nil, nil)

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		resp, _ := doJSON(t, method, front+"/v1/sessions/ws_does_not_exist", "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s status = %d, want 404", method, resp.StatusCode)
		}
	}
}

func TestSessionCreateRejectsAnUnusableWorkspace(t *testing.T) {
	front, _ := newAgentServer(t, nil, nil)

	resp, _ := doJSON(t, http.MethodPost, front+"/v1/sessions",
		`{"workspace":"/no/such/directory/anywhere"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSessionCreateRejectsUnknownFields(t *testing.T) {
	// A typo'd key should fail loudly rather than be silently ignored.
	front, _ := newAgentServer(t, nil, nil)

	resp, _ := doJSON(t, http.MethodPost, front+"/v1/sessions",
		`{"workspase":"/tmp"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestSessionCreatedExplicitlyCanRunAChatRequest(t *testing.T) {
	// The reason these endpoints exist: claim a workspace before there is
	// anything to say to the model, then use it.
	front, dir := newAgentServer(t, map[string]string{"a.txt": "one\n"}, []string{
		toolTurn("c1", "read", `{"path":"a.txt"}`),
		`{"role":"assistant","content":"It says one."}`,
	})

	_, created := doJSON(t, http.MethodPost, front+"/v1/sessions",
		`{"workspace":"`+dir+`"}`)
	id, _ := created["id"].(string)

	// Bound through the header channel rather than the body, to exercise it
	// end to end.
	req, _ := http.NewRequest(http.MethodPost, front+"/v1/chat/completions",
		strings.NewReader(`{"model":"stub","messages":[{"role":"user","content":"read it"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSession, id)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d: %s", resp.StatusCode, b)
	}
	if resp.Header.Get(HeaderSession) != id {
		t.Errorf("the run used a different session: %q", resp.Header.Get(HeaderSession))
	}
}

func TestSessionBoundThroughModelSuffix(t *testing.T) {
	// The channel that survives every gateway, because model is required and
	// nothing interprets it.
	front, dir := newAgentServer(t, map[string]string{"a.txt": "one\n"}, []string{
		toolTurn("c1", "read", `{"path":"a.txt"}`),
		`{"role":"assistant","content":"It says one."}`,
	})

	_, created := doJSON(t, http.MethodPost, front+"/v1/sessions",
		`{"workspace":"`+dir+`"}`)
	id, _ := created["id"].(string)

	resp, body := doJSON(t, http.MethodPost, front+"/v1/chat/completions",
		`{"model":"stub::`+id+`","messages":[{"role":"user","content":"read it"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", resp.StatusCode, body)
	}
	if resp.Header.Get(HeaderSession) != id {
		t.Errorf("session = %q, want %q", resp.Header.Get(HeaderSession), id)
	}
	// The suffix must not survive into the reported model.
	if body["model"] != "stub" {
		t.Errorf("model = %v, want the suffix stripped", body["model"])
	}
}
