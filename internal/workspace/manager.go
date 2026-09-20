package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/FirePing32/harness-api/internal/config"
)

// Manager owns the live sessions and reclaims the ones nobody is using.
type Manager struct {
	// root is where ephemeral workspaces are created, absolute and unresolved.
	// Stored in the form used to create session directories so the deletion
	// guard can compare like with like.
	root    string
	idleTTL time.Duration
	log     *slog.Logger

	mu       sync.Mutex
	sessions map[string]*Session
	closed   bool
}

// sessionIDPattern is the exact shape NewSessionID produces. The deletion guard
// checks against it, so a value that reached the manager from anywhere else can
// never name a directory to remove.
var sessionIDPattern = regexp.MustCompile(`^ws_[0-9a-f]{32}$`)

// ErrNotFound is returned for an unknown session id.
var ErrNotFound = errors.New("session not found")

// NewManager prepares the session registry and its workspace root.
func NewManager(cfg config.Workspace, log *slog.Logger) (*Manager, error) {
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root %q: %w", cfg.Root, err)
	}
	// 0o700: workspaces hold whatever the agent was asked to work on, and on a
	// shared machine that is nobody else's business.
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace root %q: %w", root, err)
	}

	return &Manager{
		root:     root,
		idleTTL:  cfg.IdleTTL.Duration(),
		log:      log,
		sessions: make(map[string]*Session),
	}, nil
}

// Root is the parent directory for ephemeral workspaces.
func (m *Manager) Root() string { return m.root }

// NewSessionID returns an unguessable session identifier. A session id is a
// handle to a directory the holder can read and write, so it is generated the
// way a credential is, not with a counter.
func NewSessionID() string {
	var b [16]byte
	// crypto/rand.Read cannot fail on any supported platform; it panics internally
	// if the system source is broken, which is the correct outcome here too.
	rand.Read(b[:])
	return "ws_" + hex.EncodeToString(b[:])
}

// CreateEphemeral makes a session in a fresh directory owned by this server.
// The directory is removed when the session is evicted or closed.
func (m *Manager) CreateEphemeral() (*Session, error) {
	id := NewSessionID()
	dir := filepath.Join(m.root, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace for %s: %w", id, err)
	}

	s, err := m.adopt(id, dir, true)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return s, nil
}

// CreateAt makes a session bound to an existing directory the caller nominated.
// Such a session is never ephemeral: this is somebody's working tree and the
// server has no business deleting it, whatever happens to the session.
func (m *Manager) CreateAt(dir string) (*Session, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace %q is not usable: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace %q is not a directory", dir)
	}
	return m.adopt(NewSessionID(), abs, false)
}

func (m *Manager) adopt(id, dir string, ephemeral bool) (*Session, error) {
	jail, err := OpenJail(dir)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	s := &Session{
		id:        id,
		jail:      jail,
		ledger:    NewLedger(),
		ignore:    LoadIgnore(jail),
		ephemeral: ephemeral,
		dir:       dir,
		createdAt: now,
		lastUsed:  now,
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		jail.Close()
		return nil, errors.New("workspace manager is shut down")
	}
	m.sessions[id] = s
	m.mu.Unlock()

	m.log.Info("session created",
		"session_id", id, "workspace", jail.Path(), "ephemeral", ephemeral)
	return s, nil
}

// Get returns a live session and marks it as used.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	m.mu.Unlock()
	if ok {
		s.Touch()
	}
	return s, ok
}

// Len reports how many sessions are live.
func (m *Manager) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Remove closes a session and, if it owns its directory, deletes it.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return m.destroy(s)
}

// Sweep evicts sessions idle for longer than the TTL and returns the count.
// A session currently serving a request is never evicted, however long it has
// been running: the lock is held for the whole agent loop, so a ten-minute run
// would otherwise have its workspace deleted underneath it.
func (m *Manager) Sweep(now time.Time) int {
	m.mu.Lock()
	var expired []*Session
	for id, s := range m.sessions {
		if now.Sub(s.LastUsed()) < m.idleTTL {
			continue
		}
		if !s.TryLock() {
			continue // in use
		}
		s.Unlock()
		expired = append(expired, s)
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	for _, s := range expired {
		if err := m.destroy(s); err != nil {
			m.log.Error("evict session", "session_id", s.id, "error", err)
			continue
		}
		m.log.Info("session evicted", "session_id", s.id,
			"idle", now.Sub(s.LastUsed()).Round(time.Second))
	}
	return len(expired)
}

// Run sweeps periodically until the context is cancelled.
func (m *Manager) Run(ctx context.Context) {
	interval := m.idleTTL / 4
	if interval < time.Minute {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.Sweep(now)
		}
	}
}

// Close shuts down the manager and releases every session.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	var errs []error
	for _, s := range all {
		if err := m.destroy(s); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// destroy releases a session's resources and deletes its directory if, and only
// if, this server created that directory.
//
// This is the one place in the codebase that removes a directory tree
// recursively, and the cost of a mistake here is somebody's uncommitted work.
// So the guard does not trust the session's ephemeral flag on its own: it
// re-derives the path it is willing to delete from the manager's own root and
// the session id, requires the id to match the exact shape this server
// generates, and deletes only if the result is identical to the recorded path.
// A session created by CreateAt cannot satisfy any of it.
func (m *Manager) destroy(s *Session) error {
	closeErr := s.jail.Close()

	if !s.ephemeral {
		return closeErr
	}
	if !sessionIDPattern.MatchString(s.id) {
		return errors.Join(closeErr, fmt.Errorf(
			"refusing to remove workspace for session %q: the id is not one this server generated", s.id))
	}

	want := filepath.Join(m.root, s.id)
	if s.dir != want {
		return errors.Join(closeErr, fmt.Errorf(
			"refusing to remove %q: it is not where this server puts ephemeral workspaces (%q)", s.dir, want))
	}

	if err := os.RemoveAll(want); err != nil {
		return errors.Join(closeErr, fmt.Errorf("remove workspace %q: %w", want, err))
	}
	return closeErr
}
