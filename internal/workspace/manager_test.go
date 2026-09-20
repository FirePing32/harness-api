package workspace

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FirePing32/harness-api/internal/config"
)

func newManager(t *testing.T, ttl time.Duration) *Manager {
	t.Helper()
	m, err := NewManager(config.Workspace{
		Root:    t.TempDir(),
		IdleTTL: config.Duration(ttl),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestManagerEphemeralSessionLifecycle(t *testing.T) {
	m := newManager(t, time.Hour)

	s, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Ephemeral() {
		t.Error("a manager-created session should be ephemeral")
	}
	if _, err := os.Stat(s.Root()); err != nil {
		t.Fatalf("the workspace directory was not created: %v", err)
	}

	got, ok := m.Get(s.ID())
	if !ok || got != s {
		t.Fatal("Get did not return the session")
	}

	dir := s.Root()
	if err := m.Remove(s.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the ephemeral workspace directory survived removal")
	}
	if _, ok := m.Get(s.ID()); ok {
		t.Error("a removed session is still reachable")
	}
}

func TestManagerNeverDeletesACallerSuppliedWorkspace(t *testing.T) {
	// The single most damaging bug this package could have. A session bound to
	// somebody's working tree must leave it alone on every exit path.
	m := newManager(t, time.Hour)

	project := t.TempDir()
	keep := filepath.Join(project, "important.go")
	if err := os.WriteFile(keep, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := m.CreateAt(project)
	if err != nil {
		t.Fatal(err)
	}
	if s.Ephemeral() {
		t.Fatal("a caller-supplied workspace must never be marked ephemeral")
	}

	if err := m.Remove(s.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("removing the session destroyed the caller's files: %v", err)
	}

	// ...and again via the other two paths that destroy sessions.
	s2, err := m.CreateAt(project)
	if err != nil {
		t.Fatal(err)
	}
	s2.stateMu.Lock()
	s2.lastUsed = time.Now().Add(-24 * time.Hour)
	s2.stateMu.Unlock()
	m.Sweep(time.Now())
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("eviction destroyed the caller's files: %v", err)
	}

	if _, err := m.CreateAt(project); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("shutdown destroyed the caller's files: %v", err)
	}
}

func TestManagerDeletionGuardRejectsATamperedPath(t *testing.T) {
	// The ephemeral flag alone is not trusted. If the recorded directory is not
	// exactly where this manager puts ephemeral workspaces, nothing is removed.
	m := newManager(t, time.Hour)

	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "data.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	s.dir = victim // as if something had corrupted the record

	if err := m.destroy(s); err == nil {
		t.Fatal("destroy accepted a directory outside the manager's root")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a directory outside the manager's root was removed: %v", err)
	}
}

func TestManagerDeletionGuardRejectsAForeignSessionID(t *testing.T) {
	m := newManager(t, time.Hour)

	s, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.id = "../../../etc"

	if err := m.destroy(s); err == nil {
		t.Fatal("destroy accepted an id this server did not generate")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the real workspace was removed despite the guard firing: %v", err)
	}
}

func TestManagerSweepEvictsIdleSessions(t *testing.T) {
	m := newManager(t, 50*time.Millisecond)

	idle, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	dir := idle.Root()

	fresh, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}

	idle.stateMu.Lock()
	idle.lastUsed = time.Now().Add(-time.Hour)
	idle.stateMu.Unlock()

	if n := m.Sweep(time.Now()); n != 1 {
		t.Fatalf("Sweep evicted %d sessions, want 1", n)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the evicted workspace directory survived")
	}
	if _, ok := m.Get(fresh.ID()); !ok {
		t.Error("a recently used session was evicted")
	}
}

func TestManagerSweepSkipsSessionsInUse(t *testing.T) {
	// A long agent loop holds its session for the whole request. Evicting it
	// mid-run would delete the workspace out from under the tools.
	m := newManager(t, time.Millisecond)

	s, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	s.stateMu.Lock()
	s.lastUsed = time.Now().Add(-time.Hour)
	s.stateMu.Unlock()

	s.Lock()
	n := m.Sweep(time.Now())
	s.Unlock()

	if n != 0 {
		t.Fatalf("Sweep evicted %d in-use sessions, want 0", n)
	}
	if _, ok := m.Get(s.ID()); !ok {
		t.Error("an in-use session was dropped from the registry")
	}
	if _, err := os.Stat(s.Root()); err != nil {
		t.Errorf("an in-use workspace was removed: %v", err)
	}
}

func TestManagerGetTouchesTheSession(t *testing.T) {
	m := newManager(t, time.Hour)
	s, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}

	s.stateMu.Lock()
	s.lastUsed = time.Now().Add(-time.Hour)
	s.stateMu.Unlock()

	before := s.LastUsed()
	m.Get(s.ID())
	if !s.LastUsed().After(before) {
		t.Error("Get should defer eviction by marking the session used")
	}
}

func TestManagerGetUnknownID(t *testing.T) {
	m := newManager(t, time.Hour)
	if _, ok := m.Get("ws_deadbeef"); ok {
		t.Error("Get returned a session for an unknown id")
	}
	if err := m.Remove("ws_deadbeef"); err == nil {
		t.Error("Remove accepted an unknown id")
	}
}

func TestManagerSessionsAreIsolated(t *testing.T) {
	m := newManager(t, time.Hour)

	a, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.CreateEphemeral()
	if err != nil {
		t.Fatal(err)
	}

	if a.Root() == b.Root() {
		t.Fatal("two sessions share a workspace directory")
	}
	if err := a.Jail().WriteFile("secret.txt", []byte("a's data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Jail().ReadFile("secret.txt"); err == nil {
		t.Error("one session could read another's file")
	}
	if _, err := b.Jail().ReadFile("../" + filepath.Base(a.Root()) + "/secret.txt"); err == nil {
		t.Error("one session reached another's workspace by traversal")
	}
}

func TestNewSessionIDIsUnguessableAndWellShaped(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		id := NewSessionID()
		if !sessionIDPattern.MatchString(id) {
			t.Fatalf("id %q does not match the generated shape", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	m := newManager(t, time.Hour)
	if _, err := m.CreateEphemeral(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close returned %v", err)
	}
	if _, err := m.CreateEphemeral(); err == nil {
		t.Error("a closed manager still creates sessions")
	}
}

func TestManagerRejectsNonDirectoryWorkspace(t *testing.T) {
	m := newManager(t, time.Hour)

	file := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateAt(file); err == nil {
		t.Error("CreateAt accepted a regular file")
	}
	if _, err := m.CreateAt(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("CreateAt accepted a path that does not exist")
	}
}
