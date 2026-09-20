package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newJail builds a jail over a fresh temp directory containing a known file.
func newJail(t *testing.T) (*Jail, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("in\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJail(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j, dir
}

func TestJailRejectsParentTraversal(t *testing.T) {
	j, _ := newJail(t)

	for _, name := range []string{
		"..",
		"../escape.txt",
		"../../etc/passwd",
		"a/../../escape.txt",
		"./../../escape.txt",
	} {
		if _, err := j.Rel(name); err == nil {
			t.Errorf("Rel(%q) was accepted; it leaves the workspace", name)
		} else {
			var pe *PathError
			if !errors.As(err, &pe) || pe.Code != CodePathOutside {
				t.Errorf("Rel(%q) error = %v, want a PATH_OUTSIDE_WORKSPACE PathError", name, err)
			}
		}
	}
}

func TestJailAllowsDotDotWithinTree(t *testing.T) {
	// "a/../inside.txt" never leaves the root, so rejecting it would be wrong —
	// models generate this constantly when composing paths.
	j, _ := newJail(t)

	rel, err := j.Rel("a/../inside.txt")
	if err != nil {
		t.Fatalf("Rel rejected a path that stays inside the root: %v", err)
	}
	if rel != "inside.txt" {
		t.Errorf("Rel = %q, want inside.txt", rel)
	}
}

func TestJailRejectsNamesThatMerelyStartWithDots(t *testing.T) {
	// The traversal check must be on whole path components. A strings.Contains
	// test for ".." would reject these perfectly ordinary names.
	j, _ := newJail(t)

	for _, name := range []string{"..config", "a/..hidden", "..config/x.go", "x..y"} {
		if _, err := j.Rel(name); err != nil {
			t.Errorf("Rel(%q) was rejected, but it never leaves the root: %v", name, err)
		}
	}
}

func TestJailRejectsAbsolutePathOutside(t *testing.T) {
	j, _ := newJail(t)

	if _, err := j.Rel("/etc/passwd"); err == nil {
		t.Fatal("an absolute path outside the workspace was accepted")
	}
}

func TestJailAcceptsAbsolutePathInside(t *testing.T) {
	// Tool output shows absolute paths in some places; the model will send them back.
	j, dir := newJail(t)

	rel, err := j.Rel(filepath.Join(dir, "inside.txt"))
	if err != nil {
		t.Fatalf("an absolute path inside the workspace was rejected: %v", err)
	}
	if rel != "inside.txt" {
		t.Errorf("Rel = %q, want inside.txt", rel)
	}
}

func TestJailAcceptsUnresolvedRootAlias(t *testing.T) {
	// On macOS t.TempDir() hands back /var/... which resolves to /private/var/....
	// Both spellings reach the same file and both will appear in a conversation.
	if runtime.GOOS != "darwin" {
		t.Skip("no divergent root spelling on this platform")
	}
	j, dir := newJail(t)
	if j.alias == "" {
		t.Skip("root did not resolve to a different path")
	}
	if dir != j.alias {
		t.Fatalf("alias = %q, want the unresolved dir %q", j.alias, dir)
	}

	if _, err := j.Rel(filepath.Join(j.alias, "inside.txt")); err != nil {
		t.Errorf("the unresolved spelling of the root was rejected: %v", err)
	}
	if _, err := j.Rel(filepath.Join(j.path, "inside.txt")); err != nil {
		t.Errorf("the resolved spelling of the root was rejected: %v", err)
	}
}

func TestJailRejectsNULByte(t *testing.T) {
	j, _ := newJail(t)

	_, err := j.Rel("inside.txt\x00.png")
	var pe *PathError
	if !errors.As(err, &pe) || pe.Code != CodePathInvalid {
		t.Fatalf("a NUL byte in a path gave %v, want a PATH_INVALID PathError", err)
	}
}

func TestJailRejectsEmptyPath(t *testing.T) {
	j, _ := newJail(t)

	if _, err := j.Rel(""); err == nil {
		t.Fatal("an empty path was accepted")
	}
}

func TestJailBlocksSymlinkToParent(t *testing.T) {
	j, dir := newJail(t)

	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	// A relative symlink that climbs out of the workspace. This passes every
	// lexical check — the name "escape/secret.txt" contains no "..".
	if err := os.Symlink("..", filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}

	if _, err := j.ReadFile("escape/secret.txt"); err == nil {
		t.Fatal("read through a symlink pointing at the parent directory succeeded")
	}
}

func TestJailBlocksSymlinkToAbsoluteOutside(t *testing.T) {
	j, dir := newJail(t)

	if err := os.Symlink("/etc", filepath.Join(dir, "etc")); err != nil {
		t.Fatal(err)
	}

	if _, err := j.ReadFile("etc/passwd"); err == nil {
		t.Fatal("read through a symlink to /etc succeeded")
	}
	if _, err := j.Stat("etc"); err == nil {
		t.Fatal("stat through a symlink to /etc succeeded")
	}
}

func TestJailBlocksSymlinkCreatedAfterValidation(t *testing.T) {
	// The reason containment is os.Root and not EvalSymlinks-then-compare: a path
	// validated as safe can be made unsafe before it is opened. Here the symlink
	// is planted between the two, which is the race an offline check cannot win.
	j, dir := newJail(t)

	outside := filepath.Join(filepath.Dir(dir), "planted.txt")
	if err := os.WriteFile(outside, []byte("planted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(outside) })

	rel, err := j.Rel("later")
	if err != nil {
		t.Fatalf("Rel rejected an ordinary name: %v", err)
	}

	if err := os.Symlink(filepath.Dir(dir), filepath.Join(dir, "later")); err != nil {
		t.Fatal(err)
	}

	if _, err := j.ReadFile(rel + "/planted.txt"); err == nil {
		t.Fatal("a symlink planted after validation was followed out of the workspace")
	}
}

func TestJailFollowsSymlinkWithinTree(t *testing.T) {
	// Containment must not mean "no symlinks at all" — repositories use them.
	j, dir := newJail(t)

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../inside.txt", filepath.Join(dir, "sub", "link.txt")); err != nil {
		t.Fatal(err)
	}

	b, err := j.ReadFile("sub/link.txt")
	if err != nil {
		t.Fatalf("a symlink pointing inside the workspace was refused: %v", err)
	}
	if string(b) != "in\n" {
		t.Errorf("content = %q, want the target's content", b)
	}
}

func TestJailRootThatIsItselfASymlink(t *testing.T) {
	// A workspace reached through a symlink is normal — /tmp is one on macOS.
	// If the root were not resolved first, every path inside it would look like
	// an escape.
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "f.txt"), []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	j, err := OpenJail(link)
	if err != nil {
		t.Fatalf("OpenJail on a symlinked root failed: %v", err)
	}
	defer j.Close()

	b, err := j.ReadFile("f.txt")
	if err != nil {
		t.Fatalf("read inside a symlinked root failed: %v", err)
	}
	if string(b) != "ok\n" {
		t.Errorf("content = %q", b)
	}

	// The resolved form is what the model should be shown.
	resolved, _ := filepath.EvalSymlinks(real)
	if j.Path() != resolved {
		t.Errorf("Path() = %q, want the resolved root %q", j.Path(), resolved)
	}
	// ...and the symlinked spelling must still be accepted on the way back in.
	if _, err := j.Rel(filepath.Join(link, "f.txt")); err != nil {
		t.Errorf("the symlinked spelling of the root was rejected: %v", err)
	}
}

func TestJailMissingLeafIsNotExistNotEscape(t *testing.T) {
	// A path whose last component does not exist yet is how every file gets
	// created. It must report "does not exist", not "outside the workspace" —
	// the two call for completely different corrections.
	j, _ := newJail(t)

	_, err := j.ReadFile("not/created/yet.txt")
	if err == nil {
		t.Fatal("reading a missing path succeeded")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want fs.ErrNotExist", err)
	}
	var pe *PathError
	if errors.As(err, &pe) {
		t.Fatalf("a missing file was reported as a path violation: %v", err)
	}
}

func TestJailErrorNamesTheCallersPath(t *testing.T) {
	// os.Root reports the relative name it was given. Echoing that back would
	// show the model a path it never wrote.
	j, dir := newJail(t)

	abs := filepath.Join(dir, "missing.txt")
	_, err := j.ReadFile(abs)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), abs) {
		t.Errorf("error = %q, want it to name %q", err, abs)
	}
}

func TestJailRefusesNonDirectoryRoot(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenJail(file); err == nil {
		t.Fatal("OpenJail accepted a regular file as a workspace root")
	}
}

func TestJailDisplayIsRelative(t *testing.T) {
	j, _ := newJail(t)

	if got := j.Display("src/main.go"); got != "src/main.go" {
		t.Errorf("Display = %q", got)
	}
	if got := j.Display("."); got != "." {
		t.Errorf("Display(.) = %q", got)
	}
	if got := j.Abs("src/main.go"); got != filepath.Join(j.Path(), "src/main.go") {
		t.Errorf("Abs = %q", got)
	}
}
