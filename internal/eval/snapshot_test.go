package eval

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func scan(t *testing.T, root string) Snapshot {
	t.Helper()
	snap, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestDiffSeesContentChangesNotTimestamps(t *testing.T) {
	// Hashes, not mtimes: an agent that rewrites a file with identical bytes
	// has not changed anything, and reporting it as a modification would make
	// every no-op edit look like work.
	root := tree(t, map[string]string{"a.txt": "same", "b.txt": "before"})
	before := scan(t, root)

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Diff(before, scan(t, root))
	if !slices.Equal(got.Modified, []string{"b.txt"}) {
		t.Errorf("Modified = %v, want [b.txt]", got.Modified)
	}
}

func TestDiffSeesCreationAndDeletion(t *testing.T) {
	root := tree(t, map[string]string{"keep.txt": "k", "gone.txt": "g"})
	before := scan(t, root)

	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := Diff(before, scan(t, root))
	if !slices.Equal(got.Created, []string{"new.txt"}) {
		t.Errorf("Created = %v", got.Created)
	}
	if !slices.Equal(got.Deleted, []string{"gone.txt"}) {
		t.Errorf("Deleted = %v", got.Deleted)
	}
	if got.Empty() || got.Total() != 2 {
		t.Errorf("Total = %d, Empty = %v", got.Total(), got.Empty())
	}
}

func TestScanIgnoresTheAgentsWorkingDirectories(t *testing.T) {
	// The shell tool spills large output to .harness/output/, and a task may
	// legitimately commit. Neither is the agent's work product, and counting
	// them would make every bash-using task look destructive.
	root := tree(t, map[string]string{
		"src.go":                  "package main",
		".harness/output/cmd.txt": "10 MB of build log",
		".git/HEAD":               "ref: refs/heads/main",
	})

	snap := scan(t, root)
	if len(snap) != 1 {
		t.Errorf("scanned %d files, want only src.go: %v", len(snap), snap)
	}
}

func TestDiffOfAnUnchangedTreeIsEmpty(t *testing.T) {
	// The property every ExpectNoChanges task rests on.
	root := tree(t, map[string]string{"a/b/c.txt": "x", "d.txt": "y"})
	if got := Diff(scan(t, root), scan(t, root)); !got.Empty() {
		t.Errorf("an untouched tree reported changes: %+v", got)
	}
}

func TestCopyTreePreservesStructureAndMode(t *testing.T) {
	// Checkers are executable files inside fixtures; a copy that drops the bit
	// turns a task into a measurement error.
	src := tree(t, map[string]string{
		"nested/deep/file.txt": "content",
		"top.txt":              "top",
	})
	if err := os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "copy")
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(filepath.Join(dst, "nested/deep/file.txt"))
	if err != nil || string(body) != "content" {
		t.Fatalf("nested file: %q, %v", body, err)
	}

	info, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the executable bit was lost: %v", info.Mode())
	}

	// And the copy is independent of the original.
	if err := os.WriteFile(filepath.Join(dst, "top.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(src, "top.txt"))
	if err != nil || string(original) != "top" {
		t.Errorf("the source was affected: %q, %v", original, err)
	}
}

func TestCopyTreeRefusesToOverwrite(t *testing.T) {
	src := tree(t, map[string]string{"a.txt": "x"})
	dst := tree(t, map[string]string{"existing.txt": "leftovers"})

	if err := CopyTree(src, dst); err == nil {
		t.Fatal("copied over an existing directory; a repetition would have " +
			"started from the previous one's leftovers")
	}
}

func TestCopyTreeCarriesSymlinksAsLinks(t *testing.T) {
	// Repositories use them, and resolving one into a copy of its target
	// changes what the agent is looking at.
	src := tree(t, map[string]string{"real.txt": "content"})
	if err := os.Symlink("real.txt", filepath.Join(src, "alias.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "copy")
	if err := CopyTree(src, dst); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(filepath.Join(dst, "alias.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was resolved into a regular file")
	}
}
