package workspace

import (
	"crypto/sha256"
	"fmt"
	"sync"
	"time"
)

// The read-before-edit rule, as a compare-and-swap rather than a flag.
//
// The naive version records a boolean per path: "has the model read this?" It
// stops the model from editing a file it has never seen, which is the common
// failure, and nothing else. It does not stop the model from reading a file,
// running a shell command that rewrites it, and then editing it against the
// contents it remembers — which silently discards the shell command's work.
//
// Recording a content hash turns the rule into a version check. An edit is
// authorised only if the file is still byte-for-byte what was observed, so any
// change from any source invalidates the observation and forces a re-read.
//
// Two cases are easy to get wrong, and both are handled here:
//
//   - Creation is not gated on a prior read, and used to be. The rule was that
//     a create needed a confirmed absence, to stop two creators racing. It
//     never bought that: the caller stats the path under the session lock
//     immediately before asking, so "not there" holds at the moment of the
//     write and creating it destroys nothing. Requiring an earlier read only
//     widened the window — one turn apart instead of a few microseconds.
//
//     It did cost a turn on every file creation, measured against a real
//     model, and saying so in the system prompt did not stop the model going
//     straight to write. What still matters, and is still enforced, is that
//     absence is *checked* at write time: a path that turns out to exist is
//     refused unless its contents were read.
//
//   - Compaction invalidates observations. When the summariser drops the turn
//     that contained a file's contents, the model no longer has those contents,
//     but the ledger entry survives and keeps saying yes. The invariant quietly
//     degrades into a rubber stamp. MarkStaleBefore exists to prevent that.

// Observation is what a session knows about one path at one point in time.
type Observation struct {
	// Path is workspace-relative.
	Path string
	// Exists distinguishes a read from a confirmed absence.
	Exists bool
	// Hash is the SHA-256 of the observed content. Zero when Exists is false.
	Hash [32]byte
	Size int64
	// ModTime is recorded for diagnostics only. Never for the version check:
	// many filesystems have coarse timestamps, and a fast rewrite can produce an
	// identical mtime with different bytes.
	ModTime time.Time
	// Turn is the message index the observation came from, so compaction can
	// identify the entries whose evidence it is about to discard.
	Turn int
	// Stale marks an observation whose originating message is gone from context.
	Stale bool
}

// Ledger records what a session has observed of its workspace.
// It is safe for concurrent use; read tools run in parallel.
type Ledger struct {
	mu      sync.Mutex
	entries map[string]Observation
}

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{entries: make(map[string]Observation)}
}

// Observation error codes. These travel to the model in the tool result, so the
// accompanying message has to say what to do, not merely what went wrong.
const (
	CodeNotObserved = "FS_NOT_OBSERVED"
	CodeStale       = "FS_STALE_VERSION"
)

// ObservationError rejects a modification the ledger cannot authorise.
type ObservationError struct {
	Code   string
	Path   string
	Reason string
}

func (e *ObservationError) Error() string { return e.Reason }

// Observe records the content of a path that was read successfully.
func (l *Ledger) Observe(path string, content []byte, modTime time.Time, turn int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[path] = Observation{
		Path:    path,
		Exists:  true,
		Hash:    sha256.Sum256(content),
		Size:    int64(len(content)),
		ModTime: modTime,
		Turn:    turn,
	}
}

// ObserveAbsent records that a path was checked and found not to exist. This is
// what keeps a later create honest if something else gets there first.
func (l *Ledger) ObserveAbsent(path string, turn int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[path] = Observation{Path: path, Turn: turn}
}

// Lookup returns the recorded observation for a path.
func (l *Ledger) Lookup(path string) (Observation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	obs, ok := l.entries[path]
	return obs, ok
}

// Forget drops a path's observation, for instance after it is deleted.
func (l *Ledger) Forget(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, path)
}

// Len reports how many paths are tracked.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// MarkStaleBefore invalidates every observation made before turn, and reports
// how many it touched. Call this after compaction drops messages: the model no
// longer holds the contents those reads produced, so the ledger must stop
// vouching for them.
//
// Entries are marked rather than deleted so the resulting error can say the
// file was read but the contents are gone, which tells the model to re-read.
// A bare "has not been read" after it plainly did read the file reads as a bug
// and invites the model to argue with the tool instead of retrying.
func (l *Ledger) MarkStaleBefore(turn int) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	n := 0
	for path, obs := range l.entries {
		if obs.Turn < turn && !obs.Stale {
			obs.Stale = true
			l.entries[path] = obs
			n++
		}
	}
	return n
}

// MarkStale invalidates specific paths, for when their contents were dropped
// from the conversation individually rather than by a wholesale summary.
//
// Marked rather than deleted, for the same reason as MarkStaleBefore: the
// model did read the file, and telling it otherwise invites an argument with
// the tool instead of a re-read.
func (l *Ledger) MarkStale(paths ...string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	n := 0
	for _, path := range paths {
		obs, ok := l.entries[path]
		if !ok || obs.Stale {
			continue
		}
		obs.Stale = true
		l.entries[path] = obs
		n++
	}
	return n
}

// Authorize reports whether modifying path is permitted, given the file's
// current content. Pass exists=false with nil content for a path that is not
// there now.
//
// The caller must read the current state and call this while holding the
// session lock, so that nothing changes between the check and the write.
func (l *Ledger) Authorize(path string, current []byte, exists bool) error {
	l.mu.Lock()
	obs, ok := l.entries[path]
	l.mu.Unlock()

	switch {
	case !ok && !exists:
		// Creating a file that is not there. Permitted without a prior read,
		// and this used to be refused.
		//
		// The rule was that creation needed a confirmed absence, to avoid
		// clobbering a concurrent creator. It does not buy that. The caller
		// holds the session lock and has just stat'd the path, so "not there"
		// is established at the moment of the write — creating it destroys
		// nothing. Requiring an earlier read actually *widened* the race it was
		// meant to close: observing absence on one turn and writing on the next
		// is a far larger window than checking and writing inside one call.
		//
		// What it did buy was one wasted turn on every file creation. Measured
		// against a real model: write, refused, read, write — four turns and
		// 10k tokens for one file. Stating the rule in the system prompt did
		// not stop the model going straight to write, so the cost was not
		// recoverable by explaining it better.
		//
		// Every case where data can actually be lost is still refused below:
		// an existing file that was never read, one whose contents changed
		// since, and one whose observation was summarised away.
		return nil

	case !ok:
		// Edit-shaped wording, because edit is the caller that sees it: write
		// rewrites this for the create case in annotateWriteAuthError, where
		// the caller-facing display path is also available. Saying it in both
		// places would be two copies of one decision, free to drift.
		return &ObservationError{
			Code: CodeNotObserved, Path: path,
			Reason: fmt.Sprintf("cannot modify %q: the file has not been read in this session. "+
				"Read it first, then retry the edit.", path),
		}

	case obs.Stale:
		return &ObservationError{
			Code: CodeNotObserved, Path: path,
			Reason: fmt.Sprintf("cannot modify %q: it was read earlier, but that part of the "+
				"conversation has been summarised away and its contents are no longer available. "+
				"Read it again, then retry the edit.", path),
		}

	case !obs.Exists && !exists:
		// Confirmed absent, still absent. This is the create path.
		return nil

	case !obs.Exists && exists:
		return &ObservationError{
			Code: CodeStale, Path: path,
			Reason: fmt.Sprintf("cannot create %q: it did not exist when it was checked, but it "+
				"exists now. Something else created it. Read it before deciding what to write, "+
				"so its contents are not lost.", path),
		}

	case obs.Exists && !exists:
		return &ObservationError{
			Code: CodeStale, Path: path,
			Reason: fmt.Sprintf("cannot modify %q: it existed when it was read but has since been "+
				"deleted. Read it again to confirm its current state, then retry.", path),
		}

	case sha256.Sum256(current) != obs.Hash:
		return &ObservationError{
			Code: CodeStale, Path: path,
			Reason: fmt.Sprintf("cannot modify %q: the file has changed since it was read "+
				"(%d bytes then, %d bytes now). Editing it against the old contents would discard "+
				"that change. Read it again, then retry.", path, obs.Size, len(current)),
		}
	}

	return nil
}
