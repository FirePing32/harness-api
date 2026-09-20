package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/FirePing32/harness-api/internal/workspace"
)

// Write creates a file or replaces its entire contents.
type Write struct{}

// NewWrite builds the write tool.
func NewWrite() *Write { return &Write{} }

func (*Write) Name() string { return "write" }

func (*Write) Description() string {
	return "Create a file, or replace the whole contents of an existing one.\n\n" +
		"Before overwriting a file that already exists, read it — this tool " +
		"refuses to discard contents nobody has looked at. Parent directories " +
		"are created as needed.\n\n" +
		"To change part of a file, use edit. Rewriting a whole file to alter a " +
		"few lines risks losing everything you did not reproduce exactly."
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (*Write) Parameters(SchemaDialect) json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file, relative to the workspace root."
    },
    "content": {
      "type": "string",
      "description": "The complete contents to write."
    }
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`)
}

// ConcurrencySafe is false. A write is a barrier: anything reading the same
// path concurrently would see a torn or stale view.
func (*Write) ConcurrencySafe(json.RawMessage) bool { return false }

// WriteResult is the structured outcome of a write.
type WriteResult struct {
	Path        string `json:"path"`
	DisplayPath string `json:"display_path"`
	Bytes       int    `json:"bytes"`
	Lines       int    `json:"lines"`
	Created     bool   `json:"created"`
	// PreviousBytes is the size of what was replaced, so the model can notice
	// when it has just shrunk a file by an order of magnitude.
	PreviousBytes int `json:"previous_bytes,omitempty"`
}

func (*Write) Execute(_ context.Context, s *workspace.Session, raw json.RawMessage) (any, error) {
	var args writeArgs
	if err := parseArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Path == "" {
		return nil, Errorf(CodeInvalidArgs, "path is required.")
	}

	j := s.Jail()
	rel, err := j.Rel(args.Path)
	if err != nil {
		return nil, err
	}

	existing, readErr := j.ReadFile(args.Path)
	exists := readErr == nil

	switch {
	case exists:
		// Fall through to the ledger check below.
	case errors.Is(readErr, fs.ErrNotExist):
		existing = nil
	default:
		if info, statErr := j.Stat(args.Path); statErr == nil && info.IsDir() {
			return nil, Errorf(CodeIsDirectory, "%s is a directory.", j.Display(rel)).
				WithHint("Give a path to a file, not a directory.")
		}
		return nil, Errorf(CodeIO, "could not open %s: %s", j.Display(rel), readErr)
	}

	// The same authorisation as edit, and for the same reason. Creating a file
	// requires a confirmed absence; replacing one requires having seen what is
	// being discarded, still unchanged.
	if err := s.Ledger().Authorize(rel, existing, exists); err != nil {
		return nil, annotateWriteAuthError(err, j.Display(rel), exists)
	}

	if dir := path.Dir(rel); dir != "." && dir != "" {
		if err := j.MkdirAll(dir, 0o755); err != nil {
			return nil, Errorf(CodeIO, "could not create the directory %s: %s", dir, err)
		}
	}

	perm := fs.FileMode(0o644)
	if info, err := j.Stat(args.Path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := j.WriteFile(args.Path, []byte(args.Content), perm); err != nil {
		return nil, Errorf(CodeIO, "could not write %s: %s", j.Display(rel), err)
	}

	content := []byte(args.Content)
	var modTime time.Time
	if info, err := j.Stat(args.Path); err == nil {
		modTime = info.ModTime()
	}
	s.Ledger().Observe(rel, content, modTime, s.Turn())

	return &WriteResult{
		Path:          rel,
		DisplayPath:   j.Display(rel),
		Bytes:         len(content),
		Lines:         len(splitLines(content)),
		Created:       !exists,
		PreviousBytes: len(existing),
	}, nil
}

// annotateWriteAuthError rewrites the ledger's edit-shaped wording for the
// create case. "cannot modify a file that has not been read" is confusing
// advice when the model is trying to create something that does not exist.
func annotateWriteAuthError(err error, displayPath string, exists bool) error {
	var oe *workspace.ObservationError
	if !errors.As(err, &oe) || exists {
		return err
	}
	return &workspace.ObservationError{
		Code: oe.Code, Path: oe.Path,
		Reason: fmt.Sprintf("cannot create %s: whether it already exists has not been "+
			"checked in this session. Read it first — if it does not exist, the read will "+
			"say so and the write will then be allowed.", displayPath),
	}
}

func (*Write) Render(_ json.RawMessage, result any) string {
	r, ok := result.(*WriteResult)
	if !ok {
		return ""
	}

	var b strings.Builder
	if r.Created {
		fmt.Fprintf(&b, "Created %s (%d lines, %s).",
			r.DisplayPath, r.Lines, HumanBytes(int64(r.Bytes)))
		return b.String()
	}

	fmt.Fprintf(&b, "Replaced the contents of %s (%d lines, %s).",
		r.DisplayPath, r.Lines, HumanBytes(int64(r.Bytes)))

	// A large shrink is nearly always a model reproducing a file from memory and
	// dropping most of it. It is not an error, so the write stands, but it is
	// worth putting in front of the model while it can still be undone.
	if r.PreviousBytes > 0 && r.Bytes*4 < r.PreviousBytes {
		fmt.Fprintf(&b, "\n\nNote: the file was %s before this write and is now %s. "+
			"If you meant to change only part of it, use edit — rewriting a whole file "+
			"drops anything not reproduced exactly.",
			HumanBytes(int64(r.PreviousBytes)), HumanBytes(int64(r.Bytes)))
	}
	return b.String()
}
