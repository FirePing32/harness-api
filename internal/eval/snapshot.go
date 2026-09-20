package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// What the agent did to the filesystem, which is the only account of a run
// that cannot be talked around.
//
// A transcript shows what the model said it was doing. The checker shows
// whether the one thing the task asked for happened. Neither shows collateral
// damage: a refactor that also truncated an unrelated file passes its check
// and looks like a success. Hashing the tree before and after makes that
// visible for every task, not only the one task written to look for it.

// Snapshot is the content hash of every regular file in a tree, keyed by path
// relative to its root.
type Snapshot map[string]string

// Change describes what happened to a tree between two snapshots.
type Change struct {
	Created  []string `json:"created,omitempty"`
	Modified []string `json:"modified,omitempty"`
	Deleted  []string `json:"deleted,omitempty"`
}

// Empty reports whether nothing changed.
func (c Change) Empty() bool {
	return len(c.Created) == 0 && len(c.Modified) == 0 && len(c.Deleted) == 0
}

// Total is the number of paths affected.
func (c Change) Total() int {
	return len(c.Created) + len(c.Modified) + len(c.Deleted)
}

// Diff compares two snapshots of the same tree.
func Diff(before, after Snapshot) Change {
	var c Change
	for path, hash := range after {
		prev, existed := before[path]
		switch {
		case !existed:
			c.Created = append(c.Created, path)
		case prev != hash:
			c.Modified = append(c.Modified, path)
		}
	}
	for path := range before {
		if _, still := after[path]; !still {
			c.Deleted = append(c.Deleted, path)
		}
	}
	sort.Strings(c.Created)
	sort.Strings(c.Modified)
	sort.Strings(c.Deleted)
	return c
}

// Scan hashes every regular file under root.
//
// Directories the agent creates as working space are skipped: .git because a
// task may legitimately commit, and .harness because the shell tool spills
// large output there. Neither is the agent's work product, and counting them
// as changes would make every bash-using task look destructive.
func Scan(root string) (Snapshot, error) {
	snap := Snapshot{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if skipDir(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			// A symlink's target is hashed via its own path if it points inside
			// the tree, and is not our business if it points out.
			return nil
		}
		sum, hashErr := hashFile(path)
		if hashErr != nil {
			return hashErr
		}
		snap[rel] = sum
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

func skipDir(rel string) bool {
	switch filepath.Base(rel) {
	case ".git", ".harness":
		return true
	}
	return false
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CopyTree copies src into dst, which must not already exist.
//
// Each repetition gets its own copy because an agent run mutates the tree and
// repetition two would otherwise start from repetition one's leftovers — which
// would make N=3 three correlated observations rather than three independent
// ones, and hide exactly the flakiness the repetitions exist to expose.
func CopyTree(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("destination %s already exists", dst)
	}

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)

		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)

		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return copyFile(path, target, info.Mode().Perm())
		}
		// Devices, sockets and pipes have no business in a task fixture.
		return nil
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
