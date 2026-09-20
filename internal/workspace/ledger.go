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
//   - Confirmed absence is an observation. A model that reads a path, is told
//     it does not exist, and then creates it has done exactly the right thing.
//     Requiring a successful read before every write would make file creation
//     impossible. But absence must still be *checked* at write time, or two
//     creators race and one silently overwrites the other.
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
// what authorises creating it.
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
	case !ok:
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
