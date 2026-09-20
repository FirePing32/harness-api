package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/FirePing32/harness-api/internal/workspace"
)

// Read shows the contents of a file, line-numbered and paginated.
type Read struct{}

// NewRead builds the read tool.
func NewRead() *Read { return &Read{} }

func (*Read) Name() string { return "read" }

func (*Read) Description() string {
	return "Read a file from the workspace. Output is line-numbered, which is how you " +
		"refer to positions in later edits.\n\n" +
		"Reads up to " + itoa(MaxLines) + " lines at a time. For a longer file, use offset " +
		"to continue from where the previous read stopped. Lines longer than " +
		itoa(MaxLineChars) + " characters are shortened.\n\n" +
		"Read a file before editing it. If the file does not exist, say so plainly rather " +
		"than guessing at its contents."
}

type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (*Read) Parameters(SchemaDialect) json.RawMessage {
	// Flat object, primitive types only, no format or oneOf. This schema is
	// already inside the basic dialect, so both dialects get the same thing.
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to the file, relative to the workspace root."
    },
    "offset": {
      "type": "integer",
      "description": "1-based line number to start at. Defaults to the first line."
    },
    "limit": {
      "type": "integer",
      "description": "How many lines to read. Defaults to ` + itoa(MaxLines) + `."
    }
  },
  "required": ["path"],
  "additionalProperties": false
}`)
}

// ConcurrencySafe: reading changes nothing and can overlap anything that is
// also only reading. It still runs after any pending write, because writes are
// exclusive and act as barriers.
func (*Read) ConcurrencySafe(json.RawMessage) bool { return true }

// ReadResult is the structured outcome of a read.
type ReadResult struct {
	Path        string `json:"path"`
	DisplayPath string `json:"display_path"`
	Bytes       int64  `json:"bytes"`
	View        View   `json:"view"`
}

func (*Read) Execute(ctx context.Context, s *workspace.Session, raw json.RawMessage) (any, error) {
	var args readArgs
	if err := parseArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Path == "" {
		return nil, Errorf(CodeInvalidArgs, "path is required.").
			WithHint("Give a path relative to the workspace root, such as \"src/main.go\".")
	}
	if args.Offset < 0 {
		return nil, Errorf(CodeInvalidArgs, "offset must be 1 or greater; line numbering starts at 1.")
	}

	j := s.Jail()
	rel, err := j.Rel(args.Path)
	if err != nil {
		return nil, err
	}

	info, err := j.Stat(args.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A confirmed absence is still a real observation, and it is what makes
			// creating this file legal later. Recording it here is why an agent
			// can check-then-create without a separate "does it exist" tool.
			s.Ledger().ObserveAbsent(rel, s.Turn())
			return nil, Errorf(CodeNotFound, "%s does not exist.", j.Display(rel)).
				WithHint("Use glob to find the file, or write to create it.")
		}
		return nil, Errorf(CodeIO, "could not read %s: %s", j.Display(rel), err)
	}

	if info.IsDir() {
		return nil, Errorf(CodeIsDirectory, "%s is a directory, not a file.", j.Display(rel)).
			WithHint("Use glob with a pattern like \"%s/*\" to see what is in it.", j.Display(rel))
	}
	if !info.Mode().IsRegular() {
		return nil, Errorf(CodeNotRegular, "%s is not a regular file.", j.Display(rel))
	}

	content, err := j.ReadFile(args.Path)
	if err != nil {
		return nil, Errorf(CodeIO, "could not read %s: %s", j.Display(rel), err)
	}

	if IsBinary(content) {
		return nil, Errorf(CodeBinary, "%s looks like a binary file (%s) and would not be "+
			"readable as text.", j.Display(rel), HumanBytes(info.Size())).
			WithHint("If you need to inspect it, use bash with a tool suited to the format.")
	}

	// Record what was actually read. Storing the full content's hash — rather
	// than only the page that was shown — is what lets a later edit detect that
	// the file changed underneath, including in the part the model never saw.
	s.Ledger().Observe(rel, content, info.ModTime(), s.Turn())

	return &ReadResult{
		Path:        rel,
		DisplayPath: j.Display(rel),
		Bytes:       info.Size(),
		View:        BuildView(content, args.Offset, args.Limit),
	}, nil
}

func (*Read) Render(_ json.RawMessage, result any) string {
	r, ok := result.(*ReadResult)
	if !ok {
		return ""
	}
	return r.View.Render(r.DisplayPath)
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
