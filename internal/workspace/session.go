package workspace

import (
	"sync"
	"time"
)

// Session is one agent's workspace: where it may read and write, what it has
// observed there, and how long it has been idle.
//
// Locking is deliberately coarse. A request holds the session for its whole
// agent loop, not per tool call. Two requests interleaving on one workspace
// would race on both the filesystem and the ledger, and the resulting failures
// — an edit rejected because a concurrent request rewrote the file between the
// read and the write — would be intermittent and nearly impossible to explain
// from a transcript. Serialising is slower only in the case where the
// alternative is wrong.
type Session struct {
	id        string
	jail      *Jail
	ledger    *Ledger
	ignore    *Ignore
	ephemeral bool

	// dir is the path as this server created it, before symlink resolution. Only
	// the manager uses it, and only to decide what it is allowed to delete.
	dir string

	// mu is held for the duration of a request.
	mu sync.Mutex

	stateMu   sync.Mutex
	createdAt time.Time
	lastUsed  time.Time
	turn      int
}

func (s *Session) ID() string      { return s.id }
func (s *Session) Jail() *Jail     { return s.jail }
func (s *Session) Ledger() *Ledger { return s.ledger }
func (s *Session) Root() string    { return s.jail.Path() }
func (s *Session) Ephemeral() bool { return s.ephemeral }

// Ignore is the session's ignore matcher, used by the file-listing tools.
func (s *Session) Ignore() *Ignore {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.ignore
}

// Lock claims the session for a request. Always pair with Unlock.
func (s *Session) Lock() { s.mu.Lock() }

// Unlock releases the session.
func (s *Session) Unlock() { s.mu.Unlock() }

// TryLock claims the session if it is free. A second concurrent request on one
// workspace is a client bug, and failing it fast with a clear message beats
// blocking until a ten-minute agent loop finishes.
func (s *Session) TryLock() bool { return s.mu.TryLock() }

// Touch marks the session as recently used, deferring idle eviction.
func (s *Session) Touch() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.lastUsed = time.Now()
}

// LastUsed reports when the session was last touched.
func (s *Session) LastUsed() time.Time {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.lastUsed
}

// CreatedAt reports when the session was created.
func (s *Session) CreatedAt() time.Time {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.createdAt
}

// Turn is the current message index, used to stamp filesystem observations so
// compaction can tell which ones it has invalidated.
func (s *Session) Turn() int {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.turn
}

// SetTurn records the current message index.
func (s *Session) SetTurn(n int) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.turn = n
}

// ReloadIgnore re-reads the workspace's ignore rules. Worth calling at the
// start of a request: an agent that just ran `git init` or edited .gitignore
// has changed which files the listing tools should show.
func (s *Session) ReloadIgnore() {
	ig := LoadIgnore(s.jail)
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.ignore = ig
}
