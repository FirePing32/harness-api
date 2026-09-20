package tools

import (
	"strings"
	"testing"

	"github.com/FirePing32/harness-api/internal/workspace"
)

func TestWriteCreatesAfterAConfirmedAbsence(t *testing.T) {
	// The read that reports "does not exist" is what authorises the create.
	s := newSession(t, nil)

	if _, _, err := run(t, NewRead(), s, map[string]any{"path": "new.go"}); err == nil {
		t.Fatal("reading a missing file should have failed")
	}

	text, out, err := run(t, NewWrite(), s, map[string]any{
		"path": "new.go", "content": "package main\n",
	})
	if err != nil {
		t.Fatalf("creating a confirmed-absent file was refused: %v", err)
	}
	if got := fileContent(t, s, "new.go"); got != "package main\n" {
		t.Errorf("content = %q", got)
	}
	if r := out.(*WriteResult); !r.Created {
		t.Error("Created should be true for a new file")
	}
	if !strings.Contains(text, "Created") {
		t.Errorf("result = %q", text)
	}
}

func TestWriteRefusesToClobberAnUnreadFile(t *testing.T) {
	// Overwriting contents nobody has looked at is the destructive case this
	// tool exists to prevent.
	s := newSession(t, map[string]string{"existing.go": "important\n"})

	_, _, err := run(t, NewWrite(), s, map[string]any{
		"path": "existing.go", "content": "replaced\n",
	})
	var oe *workspace.ObservationError
	if !errorAs(err, &oe) {
		t.Fatalf("err = %v, want an ObservationError", err)
	}
	if got := fileContent(t, s, "existing.go"); got != "important\n" {
		t.Errorf("the file was overwritten despite the refusal: %q", got)
	}
}

func TestWriteAllowsOverwriteAfterReading(t *testing.T) {
	s := newSession(t, map[string]string{"a.go": "old\n"})
	observe(t, s, "a.go")

	if _, _, err := run(t, NewWrite(), s, map[string]any{
		"path": "a.go", "content": "new\n",
	}); err != nil {
		t.Fatal(err)
	}
	if got := fileContent(t, s, "a.go"); got != "new\n" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteCreatesANewFileWithoutAPriorRead(t *testing.T) {
	// This used to be refused, on the theory that a confirmed absence stopped a
	// create from clobbering a concurrent creator. It does not: the path is
	// stat'd under the session lock immediately before this, so "not there" is
	// established at the moment of the write and creating it destroys nothing.
	//
	// The refusal did cost a turn on every file creation. Measured against a
	// real model: write, refused, read, write — four turns for one file, and
	// saying so in the system prompt did not stop it.
	s := newSession(t, nil)

	_, _, err := run(t, NewWrite(), s, map[string]any{
		"path": "brand-new.go", "content": "x\n",
	})
	if err != nil {
		t.Fatalf("creating a file that does not exist was refused: %v", err)
	}
	if got := fileContent(t, s, "brand-new.go"); got != "x\n" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteStillRefusesToClobberAfterTheCreateRelaxation(t *testing.T) {
	// The half that matters. Relaxing creation must not weaken the case where
	// bytes can actually be lost.
	s := newSession(t, map[string]string{"existing.go": "precious\n"})

	_, _, err := run(t, NewWrite(), s, map[string]any{
		"path": "existing.go", "content": "clobbered\n",
	})
	if err == nil {
		t.Fatal("overwrote an existing file nobody had read")
	}
	if got := fileContent(t, s, "existing.go"); got != "precious\n" {
		t.Errorf("the file was modified anyway: %q", got)
	}
}

func TestWriteCreatesParentDirectories(t *testing.T) {
	s := newSession(t, nil)
	if _, _, err := run(t, NewRead(), s, map[string]any{"path": "a/b/c/deep.go"}); err == nil {
		t.Fatal("expected the read to fail")
	}

	if _, _, err := run(t, NewWrite(), s, map[string]any{
		"path": "a/b/c/deep.go", "content": "x\n",
	}); err != nil {
		t.Fatalf("write did not create the parent directories: %v", err)
	}
	if got := fileContent(t, s, "a/b/c/deep.go"); got != "x\n" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteWarnsOnALargeShrink(t *testing.T) {
	// Nearly always a model reproducing a file from memory and dropping most of
	// it. The write stands, but it goes in front of the model while it can still
	// be undone.
	big := strings.Repeat("line of real content\n", 200)
	s := newSession(t, map[string]string{"a.go": big})
	observe(t, s, "a.go")

	text, _, err := run(t, NewWrite(), s, map[string]any{
		"path": "a.go", "content": "package main\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "before this write") {
		t.Errorf("a large shrink was not flagged:\n%s", text)
	}
	if !strings.Contains(text, "edit") {
		t.Errorf("the warning does not suggest edit:\n%s", text)
	}
}

func TestWriteDoesNotWarnOnAnOrdinaryRewrite(t *testing.T) {
	s := newSession(t, map[string]string{"a.go": "one\ntwo\nthree\n"})
	observe(t, s, "a.go")

	text, _, err := run(t, NewWrite(), s, map[string]any{
		"path": "a.go", "content": "one\ntwo\nfour\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "before this write") {
		t.Errorf("a same-size rewrite was flagged as a shrink:\n%s", text)
	}
}

func TestWriteUpdatesTheLedger(t *testing.T) {
	s := newSession(t, nil)
	if _, _, err := run(t, NewRead(), s, map[string]any{"path": "a.go"}); err == nil {
		t.Fatal("expected the read to fail")
	}
	if _, _, err := run(t, NewWrite(), s, map[string]any{"path": "a.go", "content": "v1\n"}); err != nil {
		t.Fatal(err)
	}

	// A write is itself an observation, so an immediate edit is allowed.
	if _, _, err := run(t, NewEdit(), s, map[string]any{
		"path": "a.go", "old_string": "v1", "new_string": "v2",
	}); err != nil {
		t.Fatalf("editing a file just written was refused: %v", err)
	}
}

func TestWriteRejectsADirectory(t *testing.T) {
	s := newSession(t, map[string]string{"src/a.go": "x\n"})

	_, _, err := run(t, NewWrite(), s, map[string]any{"path": "src", "content": "x"})
	if err == nil {
		t.Fatal("writing over a directory succeeded")
	}
}

func TestWriteRejectsEscapingPath(t *testing.T) {
	s := newSession(t, nil)

	_, _, err := run(t, NewWrite(), s, map[string]any{
		"path": "../escape.txt", "content": "x",
	})
	var pe *workspace.PathError
	if !errorAs(err, &pe) {
		t.Fatalf("err = %v, want a workspace.PathError", err)
	}
}

func TestWriteEmptyContentIsAllowed(t *testing.T) {
	// Truncating a file to nothing is a legitimate thing to ask for.
	s := newSession(t, map[string]string{"a.go": "x\n"})
	observe(t, s, "a.go")

	if _, _, err := run(t, NewWrite(), s, map[string]any{"path": "a.go", "content": ""}); err != nil {
		t.Fatal(err)
	}
	if got := fileContent(t, s, "a.go"); got != "" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteIsNotConcurrencySafe(t *testing.T) {
	if NewWrite().ConcurrencySafe(nil) {
		t.Error("write must act as a barrier, not run concurrently")
	}
}
