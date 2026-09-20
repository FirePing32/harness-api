package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/FirePing32/harness-api/internal/config"
	"github.com/FirePing32/harness-api/internal/workspace"
)

// Bash runs a shell command in the workspace.
type Bash struct {
	shell workspace.Shell
	cfg   config.Shell
}

// NewBash builds the bash tool.
func NewBash(shell workspace.Shell, cfg config.Shell) *Bash {
	return &Bash{shell: shell, cfg: cfg}
}

func (*Bash) Name() string { return "bash" }

func (b *Bash) Description() string {
	return "Run a shell command with bash.\n\n" +
		"Each call is a separate shell. Nothing carries over between calls: not " +
		"the working directory, not exported variables, not an activated " +
		"virtualenv. Use the workdir argument instead of cd, and combine steps " +
		"that depend on each other into one command with && rather than issuing " +
		"them separately.\n\n" +
		"Standard output and standard error come back interleaved, as you would " +
		"see them in a terminal, followed by the exit code when it is not zero. " +
		"Long output is truncated from the beginning, keeping the end, because " +
		"that is where errors are.\n\n" +
		"Commands time out after " + b.cfg.DefaultTimeout.String() + " by default. " +
		"For a build or a test suite that legitimately takes longer, pass " +
		"timeout_seconds.\n\n" +
		"Use the file tools rather than the shell where one fits: read instead of " +
		"cat, glob instead of find, grep instead of grep. They are faster, they " +
		"page properly, and edits made through them are checked against what you " +
		"have actually read."
}

type bashArgs struct {
	Command        string `json:"command"`
	Workdir        string `json:"workdir,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

func (b *Bash) Parameters(SchemaDialect) json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "The shell command to run."
    },
    "workdir": {
      "type": "string",
      "description": "Directory to run it in, relative to the workspace root. Defaults to the root. Use this rather than cd."
    },
    "timeout_seconds": {
      "type": "integer",
      "description": "How long to allow before the command is killed. Defaults to ` +
		itoa(int(b.cfg.DefaultTimeout.Duration().Seconds())) + `, maximum ` +
		itoa(int(b.cfg.MaxTimeout.Duration().Seconds())) + `."
    }
  },
  "required": ["command"],
  "additionalProperties": false
}`)
}

// ConcurrencySafe is false, unconditionally. A command can do anything —
// write files, start servers, change the working tree — so it is an ordering
// barrier no matter how harmless the command text looks.
func (*Bash) ConcurrencySafe(json.RawMessage) bool { return false }

// BashResult is the structured outcome of a command.
type BashResult struct {
	Command    string `json:"command"`
	Workdir    string `json:"workdir"`
	Output     string `json:"output"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	TotalBytes int    `json:"total_bytes"`
	// SpillPath, when set, is a workspace-relative file holding the complete
	// output, so the model can page through what the tail left out.
	SpillPath  string        `json:"spill_path,omitempty"`
	DurationMS int64         `json:"duration_ms"`
	timeout    time.Duration // for rendering, not serialised
}

func (b *Bash) Execute(ctx context.Context, s *workspace.Session, raw json.RawMessage) (any, error) {
	if !b.cfg.Enabled {
		return nil, Errorf(CodeDisabled, "running shell commands is disabled on this server.").
			WithHint("Use read, glob, grep, edit and write instead.")
	}

	var args bashArgs
	if err := parseArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Command) == "" {
		return nil, Errorf(CodeInvalidArgs, "command is required.")
	}

	j := s.Jail()
	dir := j.Path()
	displayDir := "."
	if args.Workdir != "" {
		rel, err := j.Rel(args.Workdir)
		if err != nil {
			return nil, err
		}
		info, err := j.Stat(args.Workdir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, Errorf(CodeNotFound, "the directory %s does not exist.", j.Display(rel)).
					WithHint("workdir must be an existing directory inside the workspace.")
			}
			return nil, Errorf(CodeIO, "could not use %s: %s", j.Display(rel), err)
		}
		if !info.IsDir() {
			return nil, Errorf(CodeIsDirectory, "%s is a file, not a directory.", j.Display(rel)).
				WithHint("workdir names the directory to run in; put the file in the command.")
		}
		dir = j.Abs(rel)
		displayDir = j.Display(rel)
	}

	timeout := b.cfg.DefaultTimeout.Duration()
	if args.TimeoutSeconds > 0 {
		requested := time.Duration(args.TimeoutSeconds) * time.Second
		if requested > b.cfg.MaxTimeout.Duration() {
			requested = b.cfg.MaxTimeout.Duration()
		}
		timeout = requested
	}

	spill := newSpillFile(s, b.cfg.TailBytes, b.cfg.SpillBytes)
	defer spill.Close()

	res, err := b.shell.Run(ctx, workspace.ShellRequest{
		Command:   args.Command,
		Dir:       dir,
		Timeout:   timeout,
		TailBytes: b.cfg.TailBytes,
		Spill:     spill,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, workspace.ErrShellUnavailable) {
			return nil, Errorf(CodeDisabled, "shell commands are not available on this server.")
		}
		return nil, Errorf(CodeIO, "could not run the command: %s", err)
	}

	// Nothing is done to the observation ledger here, deliberately. A command
	// that rewrote a file the model had read will be caught by the content hash
	// on the next edit, which produces a precise "this changed, read it again".
	// Blanket-invalidating every observation after any command would force a
	// re-read after `ls`.

	out := &BashResult{
		Command:    args.Command,
		Workdir:    displayDir,
		Output:     res.Output,
		ExitCode:   res.ExitCode,
		TimedOut:   res.TimedOut,
		Truncated:  res.Truncated,
		TotalBytes: res.TotalBytes,
		DurationMS: res.Duration.Milliseconds(),
		timeout:    timeout,
	}
	if res.Truncated {
		out.SpillPath = spill.Path()
	}
	return out, nil
}

func (*Bash) Render(_ json.RawMessage, result any) string {
	r, ok := result.(*BashResult)
	if !ok {
		return ""
	}

	var b strings.Builder
	if r.Output != "" {
		b.WriteString(strings.TrimRight(r.Output, "\n"))
		b.WriteString("\n")
	}

	var notes []string

	if r.Truncated {
		note := fmt.Sprintf("Output was %s; showing the last %s.",
			HumanBytes(int64(r.TotalBytes)), HumanBytes(int64(len(r.Output))))
		if r.SpillPath != "" {
			note += fmt.Sprintf(" The complete output is in %s — read it with an offset "+
				"or search it with grep rather than reading all of it.", r.SpillPath)
		}
		notes = append(notes, note)
	}

	switch {
	case r.TimedOut:
		// Distinguished from a non-zero exit on purpose: a timeout says nothing
		// about whether the command would have succeeded, and a model told only
		// "exit 137" will usually conclude the command is broken and rewrite it.
		notes = append(notes, fmt.Sprintf(
			"The command was still running after %s and was stopped, along with "+
				"everything it had started. It did not fail — it did not finish. "+
				"Raise timeout_seconds, or narrow the command so it does less.",
			r.timeout.Round(time.Second)))
	case r.ExitCode != 0:
		notes = append(notes, fmt.Sprintf("Exit code: %d", r.ExitCode))
	}

	if r.Output == "" && len(notes) == 0 {
		// An empty result is ambiguous — it reads as a failed call rather than a
		// command that printed nothing, which is the normal case for mv, mkdir
		// and most successful writes.
		return "The command finished with no output (exit code 0)."
	}

	if len(notes) > 0 {
		if r.Output != "" {
			b.WriteString("\n")
		}
		b.WriteString(strings.Join(notes, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// spillFile holds the complete output of a command that printed more than the
// tail limit, so the model can page through the rest.
//
// It is lazy in a way that matters: almost every command prints a few lines,
// and creating and deleting a file for each one would leave a trail of churn
// in somebody's working tree. Nothing touches the disk until the output
// actually exceeds the threshold, so the common case has no footprint at all.
type spillFile struct {
	session   *workspace.Session
	threshold int
	limit     int

	buf     []byte
	written int
	file    *os.File
	relPath string
	failed  bool
}

// spillDir is where overflow output is kept, relative to the workspace root.
// Excluded from glob and grep by default so a later search does not match the
// output of an earlier command and read it as a real result.
const spillDir = ".harness/output"

func newSpillFile(s *workspace.Session, threshold, limit int) *spillFile {
	return &spillFile{session: s, threshold: threshold, limit: limit}
}

func (f *spillFile) Write(p []byte) (int, error) {
	// Never report an error upward. Failing to keep the overflow copy must not
	// fail the command that produced it — the tail is still useful on its own.
	if f.failed || f.limit <= 0 {
		return len(p), nil
	}

	if f.file == nil {
		f.buf = append(f.buf, p...)
		if len(f.buf) <= f.threshold {
			return len(p), nil
		}
		if !f.create() {
			return len(p), nil
		}
		f.writeOut(f.buf)
		f.buf = nil
		return len(p), nil
	}

	f.writeOut(p)
	return len(p), nil
}

func (f *spillFile) create() bool {
	j := f.session.Jail()
	if err := j.MkdirAll(spillDir, 0o755); err != nil {
		f.failed = true
		return false
	}

	name := path.Join(spillDir, "cmd-"+f.session.ID()[3:11]+"-"+randomSuffix()+".log")
	file, err := j.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		f.failed = true
		return false
	}
	f.file, f.relPath = file, name
	return true
}

func (f *spillFile) writeOut(p []byte) {
	remaining := f.limit - f.written
	if remaining <= 0 {
		return
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	n, err := f.file.Write(p)
	f.written += n
	if err != nil {
		f.failed = true
	}
}

// Path is the workspace-relative path of the overflow file, empty if none was
// written.
func (f *spillFile) Path() string { return f.relPath }

func (f *spillFile) Close() error {
	if f.file == nil {
		return nil
	}
	return f.file.Close()
}

var _ io.Writer = (*spillFile)(nil)

// randomSuffix returns a short unique string for a spill file name, so two
// commands in one session cannot overwrite each other's output.
func randomSuffix() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
