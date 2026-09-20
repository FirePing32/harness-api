// Package workspace owns the filesystem a session may touch, what it has looked
// at, and how long it lives.
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Jail confines file access to a single directory tree.
//
// Containment is enforced by os.Root, which resolves every path component with
// openat relative to a held directory descriptor. That is a kernel-level check,
// not a string comparison: a symlink pointing out of the tree fails at the
// syscall, and it fails even if the symlink is created after we validated the
// path. The obvious hand-rolled alternative — filepath.EvalSymlinks followed by
// a prefix test — is a time-of-check-to-time-of-use race by construction.
//
// The lexical checks in Rel exist only to produce an error a model can act on.
// os.Root's own message is "path escapes from parent", which says nothing about
// what the model should do instead.
//
// One consequence worth knowing: os.Root refuses absolute symlinks even when
// they point back inside the tree. That is stricter than necessary, and it is
// the right trade — a workspace containing an absolute symlink to somewhere
// inside itself is rare, and loosening the rule means reimplementing the
// resolution logic that os.Root exists to provide.
type Jail struct {
	root *os.Root

	// path is the symlink-resolved absolute root. Used for display and for
	// recognising absolute paths the caller sends back. Never for enforcement.
	path string

	// alias is the unresolved form of the root when it differs from path. macOS
	// resolves /tmp to /private/tmp, so a session rooted at a temp directory has
	// two names in circulation and a model will echo back whichever it was shown.
	alias string
}

// PathError is a rejection of a path before any filesystem call is attempted.
// The message is model-facing: it says what was wrong and what to do instead.
type PathError struct {
	Path   string
	Code   string
	Reason string
}

func (e *PathError) Error() string { return e.Reason }

// Path error codes.
const (
	CodePathOutside = "PATH_OUTSIDE_WORKSPACE"
	CodePathInvalid = "PATH_INVALID"
)

// OpenJail confines access to dir, which must already exist and be a directory.
func OpenJail(dir string) (*Jail, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("workspace directory is empty")
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory %q: %w", dir, err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory %q: %w", abs, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat workspace directory %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace path %q is not a directory", resolved)
	}

	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("open workspace root %q: %w", resolved, err)
	}

	j := &Jail{root: root, path: resolved}
	if abs != resolved {
		j.alias = abs
	}
	return j, nil
}

// Path is the resolved absolute root directory.
func (j *Jail) Path() string { return j.path }

// Close releases the directory handle.
func (j *Jail) Close() error { return j.root.Close() }

// FS exposes the tree as a symlink-safe fs.FS, for walking and globbing.
func (j *Jail) FS() fs.FS { return j.root.FS() }

// Rel validates a caller-supplied path and returns it relative to the root.
//
// Both absolute and relative inputs are accepted: models mix the two freely,
// and an absolute path that happens to be inside the workspace is a reasonable
// thing to write.
func (j *Jail) Rel(name string) (string, error) {
	if name == "" {
		return "", &PathError{Path: name, Code: CodePathInvalid,
			Reason: "the path is empty; give a path relative to the workspace root, such as \"src/main.go\""}
	}
	if strings.ContainsRune(name, 0) {
		return "", &PathError{Path: name, Code: CodePathInvalid,
			Reason: "the path contains a NUL byte, which no filesystem accepts"}
	}

	if filepath.IsAbs(name) {
		cleaned := filepath.Clean(name)
		for _, base := range [2]string{j.path, j.alias} {
			if base == "" {
				continue
			}
			rel, err := filepath.Rel(base, cleaned)
			if err != nil || escapes(rel) {
				continue
			}
			return rel, nil
		}
		return "", &PathError{Path: name, Code: CodePathOutside,
			Reason: fmt.Sprintf("%q is outside the workspace; this session can only reach paths under %s", name, j.path)}
	}

	rel := filepath.Clean(name)
	if escapes(rel) {
		return "", &PathError{Path: name, Code: CodePathOutside,
			Reason: fmt.Sprintf("%q reaches above the workspace root; give a path inside %s", name, j.path)}
	}
	return rel, nil
}

// escapes reports whether a cleaned relative path leaves its base.
// The check is on whole components: a file named "..config" is perfectly legal
// and a strings.Contains test would reject it.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Display renders a workspace-relative path for the model. Relative paths keep
// tool output short and avoid leaking the host's directory layout into the
// conversation, where it would be repeated back in later requests.
func (j *Jail) Display(rel string) string {
	if rel == "." || rel == "" {
		return "."
	}
	return filepath.ToSlash(rel)
}

// Abs renders a workspace-relative path as an absolute one, for logs and for
// handing to the shell.
func (j *Jail) Abs(rel string) string {
	if rel == "." || rel == "" {
		return j.path
	}
	return filepath.Join(j.path, rel)
}

// The methods below mirror os.Root, taking a caller-supplied path and returning
// errors that name that path rather than the internal relative form.

func (j *Jail) Open(name string) (*os.File, error) {
	rel, err := j.Rel(name)
	if err != nil {
		return nil, err
	}
	f, err := j.root.Open(rel)
	return f, j.wrap(name, err)
}

func (j *Jail) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	rel, err := j.Rel(name)
	if err != nil {
		return nil, err
	}
	f, err := j.root.OpenFile(rel, flag, perm)
	return f, j.wrap(name, err)
}

func (j *Jail) Create(name string) (*os.File, error) {
	rel, err := j.Rel(name)
	if err != nil {
		return nil, err
	}
	f, err := j.root.Create(rel)
	return f, j.wrap(name, err)
}

func (j *Jail) Stat(name string) (os.FileInfo, error) {
	rel, err := j.Rel(name)
	if err != nil {
		return nil, err
	}
	info, err := j.root.Stat(rel)
	return info, j.wrap(name, err)
}

func (j *Jail) Lstat(name string) (os.FileInfo, error) {
	rel, err := j.Rel(name)
	if err != nil {
		return nil, err
	}
	info, err := j.root.Lstat(rel)
	return info, j.wrap(name, err)
}

func (j *Jail) ReadFile(name string) ([]byte, error) {
	rel, err := j.Rel(name)
	if err != nil {
		return nil, err
	}
	b, err := j.root.ReadFile(rel)
	return b, j.wrap(name, err)
}

func (j *Jail) WriteFile(name string, data []byte, perm os.FileMode) error {
	rel, err := j.Rel(name)
	if err != nil {
		return err
	}
	return j.wrap(name, j.root.WriteFile(rel, data, perm))
}

func (j *Jail) MkdirAll(name string, perm os.FileMode) error {
	rel, err := j.Rel(name)
	if err != nil {
		return err
	}
	return j.wrap(name, j.root.MkdirAll(rel, perm))
}

func (j *Jail) Remove(name string) error {
	rel, err := j.Rel(name)
	if err != nil {
		return err
	}
	return j.wrap(name, j.root.Remove(rel))
}

// ReadDir lists a directory inside the jail, sorted by name.
func (j *Jail) ReadDir(name string) ([]fs.DirEntry, error) {
	rel, err := j.Rel(name)
	if err != nil {
		return nil, err
	}
	f, err := j.root.Open(rel)
	if err != nil {
		return nil, j.wrap(name, err)
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, j.wrap(name, err)
	}
	return entries, nil
}

// wrap rewrites the path inside an *fs.PathError so the message names the path
// the caller gave, not the relative form we derived. The wrapped Err is left
// alone so errors.Is(err, fs.ErrNotExist) keeps working.
func (j *Jail) wrap(name string, err error) error {
	if err == nil {
		return nil
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		pe.Path = name
	}
	return err
}
