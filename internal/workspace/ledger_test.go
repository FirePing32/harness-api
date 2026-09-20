package workspace

import (
	"errors"
	"testing"
	"time"
)

func TestLedgerRefusesUnobservedEdit(t *testing.T) {
	l := NewLedger()

	err := l.Authorize("main.go", []byte("package main\n"), true)
	var oe *ObservationError
	if !errors.As(err, &oe) || oe.Code != CodeNotObserved {
		t.Fatalf("err = %v, want FS_NOT_OBSERVED", err)
	}
	if !contains(err.Error(), "read it first") && !contains(err.Error(), "Read it first") {
		t.Errorf("message does not say what to do next: %q", err)
	}
}

func TestLedgerAllowsEditOfUnchangedFile(t *testing.T) {
	l := NewLedger()
	content := []byte("package main\n")
	l.Observe("main.go", content, time.Now(), 1)

	if err := l.Authorize("main.go", content, true); err != nil {
		t.Fatalf("an unchanged file was refused: %v", err)
	}
}

func TestLedgerDetectsChangeUnderneath(t *testing.T) {
	// The case a boolean read-before-edit flag cannot catch: the model reads a
	// file, a shell command rewrites it, and the model then edits against what
	// it remembers — silently discarding the command's work.
	l := NewLedger()
	l.Observe("main.go", []byte("old\n"), time.Now(), 1)

	err := l.Authorize("main.go", []byte("rewritten by a build step\n"), true)
	var oe *ObservationError
	if !errors.As(err, &oe) || oe.Code != CodeStale {
		t.Fatalf("err = %v, want FS_STALE_VERSION", err)
	}
	if !contains(err.Error(), "changed since it was read") {
		t.Errorf("message does not explain the conflict: %q", err)
	}
}

func TestLedgerIdenticalContentAtDifferentTimeIsNotStale(t *testing.T) {
	// A rewrite that produced identical bytes is not a conflict. The check is on
	// content, never mtime — coarse filesystem timestamps and touch-without-change
	// would both produce false conflicts that the model cannot resolve.
	l := NewLedger()
	content := []byte("same\n")
	l.Observe("f.txt", content, time.Now().Add(-time.Hour), 1)

	if err := l.Authorize("f.txt", content, true); err != nil {
		t.Fatalf("identical content with a newer mtime was refused: %v", err)
	}
}

func TestLedgerConfirmedAbsenceAuthorisesCreation(t *testing.T) {
	// Reading a path that is not there is a successful observation, and it is
	// what makes file creation possible at all.
	l := NewLedger()
	l.ObserveAbsent("new.go", 1)

	if err := l.Authorize("new.go", nil, false); err != nil {
		t.Fatalf("creating a confirmed-absent file was refused: %v", err)
	}
}

func TestLedgerAbsenceDoesNotAuthoriseClobber(t *testing.T) {
	// Something else created the file between the check and the write. Writing
	// now would destroy contents nobody has looked at.
	l := NewLedger()
	l.ObserveAbsent("new.go", 1)

	err := l.Authorize("new.go", []byte("someone got here first\n"), true)
	var oe *ObservationError
	if !errors.As(err, &oe) || oe.Code != CodeStale {
		t.Fatalf("err = %v, want FS_STALE_VERSION", err)
	}
}

func TestLedgerDetectsDeletionSinceRead(t *testing.T) {
	l := NewLedger()
	l.Observe("gone.go", []byte("x\n"), time.Now(), 1)

	err := l.Authorize("gone.go", nil, false)
	var oe *ObservationError
	if !errors.As(err, &oe) || oe.Code != CodeStale {
		t.Fatalf("err = %v, want FS_STALE_VERSION", err)
	}
	if !contains(err.Error(), "deleted") {
		t.Errorf("message does not mention the deletion: %q", err)
	}
}

func TestLedgerCompactionInvalidatesObservations(t *testing.T) {
	// The subtle one. After the summariser drops the turn that held a file's
	// contents, the model no longer has them — but a surviving ledger entry
	// still says yes, and the read-before-edit invariant quietly becomes a
	// rubber stamp.
	l := NewLedger()
	content := []byte("package main\n")
	l.Observe("main.go", content, time.Now(), 3)
	l.Observe("other.go", content, time.Now(), 12)

	if n := l.MarkStaleBefore(10); n != 1 {
		t.Fatalf("marked %d entries stale, want 1", n)
	}

	err := l.Authorize("main.go", content, true)
	if err == nil {
		t.Fatal("an edit was authorised from an observation whose contents are gone from context")
	}
	var oe *ObservationError
	if !errors.As(err, &oe) || oe.Code != CodeNotObserved {
		t.Fatalf("err = %v, want FS_NOT_OBSERVED", err)
	}
	// The wording matters: telling the model it never read the file, when it
	// plainly did, invites it to argue with the tool instead of re-reading.
	if !contains(err.Error(), "summarised away") {
		t.Errorf("message should explain that the contents were summarised away: %q", err)
	}

	if err := l.Authorize("other.go", content, true); err != nil {
		t.Errorf("a newer observation was invalidated: %v", err)
	}
}

func TestLedgerReobservationClearsStaleness(t *testing.T) {
	l := NewLedger()
	content := []byte("x\n")
	l.Observe("main.go", content, time.Now(), 1)
	l.MarkStaleBefore(5)

	l.Observe("main.go", content, time.Now(), 7)
	if err := l.Authorize("main.go", content, true); err != nil {
		t.Fatalf("re-reading did not restore authorisation: %v", err)
	}
}

func TestLedgerForget(t *testing.T) {
	l := NewLedger()
	l.Observe("a", []byte("x"), time.Now(), 1)
	if l.Len() != 1 {
		t.Fatalf("Len = %d", l.Len())
	}
	l.Forget("a")
	if l.Len() != 0 {
		t.Errorf("Len after Forget = %d", l.Len())
	}
}

func TestLedgerConcurrentAccess(t *testing.T) {
	// Read tools run in parallel, so every ledger entry point is reachable from
	// several goroutines at once.
	l := NewLedger()
	done := make(chan struct{})
	for i := range 8 {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for j := range 50 {
				l.Observe("f", []byte{byte(i), byte(j)}, time.Now(), j)
				l.Authorize("f", []byte{byte(i), byte(j)}, true)
				l.Lookup("f")
				l.MarkStaleBefore(j)
			}
		}(i)
	}
	for range 8 {
		<-done
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
