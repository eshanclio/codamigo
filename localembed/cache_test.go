package localembed_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ieshan/codamigo/localembed"
)

func TestModelDir_RegistryModel(t *testing.T) {
	m, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	dir, err := localembed.ModelDir("/models", m)
	if err != nil {
		t.Fatalf("ModelDir: %v", err)
	}
	if want := filepath.Join("/models", "bge-small-en-v1.5"); dir != want {
		t.Errorf("ModelDir = %q, want %q", dir, want)
	}
}

func TestModelDir_RejectsBadRoot(t *testing.T) {
	m, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	for _, root := range []string{"", "relative/path", "./models"} {
		t.Run(root, func(t *testing.T) {
			if _, err := localembed.ModelDir(root, m); err == nil {
				t.Errorf("ModelDir(%q) = nil error, want rejection", root)
			}
		})
	}
}

// TestModelDir_RejectsUnsafeDirName covers the path that actually touches the
// filesystem. Lookup already constrains names, so these are defence in depth
// against a future caller building a Model by hand.
func TestModelDir_RejectsUnsafeDirName(t *testing.T) {
	for _, name := range []string{
		".",
		"..",
		"../escape",
		"a/b",
		"/absolute",
		"nested/../..",
		"with\x00null",
		"with\nnewline",
		"tab\there",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := localembed.ModelDir("/models", localembed.Model{Name: name, RepoID: "org/x"})
			if err == nil {
				t.Errorf("ModelDir with name %q = nil error, want rejection", name)
			}
		})
	}
}

// A Model with neither a name nor a repository id has no directory to resolve.
func TestModelDir_RejectsEmptyModel(t *testing.T) {
	if _, err := localembed.ModelDir("/models", localembed.Model{}); err == nil {
		t.Error("ModelDir on a zero Model = nil error, want rejection")
	}
}

func TestModelDir_EscapeAttemptStaysInsideRoot(t *testing.T) {
	// Belt and braces: even if a name slipped through, the result must not
	// resolve above the root.
	m := localembed.Model{Name: "..", RepoID: "org/x"}
	if dir, err := localembed.ModelDir("/models", m); err == nil {
		if rel, relErr := filepath.Rel("/models", dir); relErr == nil && rel == ".." {
			t.Errorf("ModelDir escaped the root: %q", dir)
		}
	}
}

func TestSnapshotDir_PinnedRevision(t *testing.T) {
	m, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	dir, err := localembed.SnapshotDir("/models/bge-small-en-v1.5", m)
	if err != nil {
		t.Fatalf("SnapshotDir: %v", err)
	}
	want := filepath.Join("/models/bge-small-en-v1.5",
		"models--BAAI--bge-small-en-v1.5", "snapshots", m.Revision)
	if dir != want {
		t.Errorf("SnapshotDir = %q, want %q", dir, want)
	}
}

func TestSnapshotDir_UnpinnedDiscoversRevision(t *testing.T) {
	m, err := localembed.Lookup("org/model")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	root := t.TempDir()
	snapshots := filepath.Join(root, "models--org--model", "snapshots", "abc123")
	if err := os.MkdirAll(snapshots, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dir, err := localembed.SnapshotDir(root, m)
	if err != nil {
		t.Fatalf("SnapshotDir: %v", err)
	}
	if dir != snapshots {
		t.Errorf("SnapshotDir = %q, want %q", dir, snapshots)
	}
}

func TestSnapshotDir_UnpinnedMissing(t *testing.T) {
	m, err := localembed.Lookup("org/model")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	_, err = localembed.SnapshotDir(t.TempDir(), m)
	if !errors.Is(err, localembed.ErrModelNotDownloaded) {
		t.Errorf("SnapshotDir on an empty root = %v, want ErrModelNotDownloaded", err)
	}
}

// TestSnapshotDir_UnpinnedAmbiguous asserts we refuse rather than guess:
// silently loading the wrong revision's weights would produce vectors that are
// incompatible with the index without any error.
func TestSnapshotDir_UnpinnedAmbiguous(t *testing.T) {
	m, err := localembed.Lookup("org/model")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	root := t.TempDir()
	for _, rev := range []string{"abc", "def"} {
		if err := os.MkdirAll(filepath.Join(root, "models--org--model", "snapshots", rev), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	if _, err := localembed.SnapshotDir(root, m); err == nil {
		t.Error("SnapshotDir with two revisions = nil error, want refusal")
	}
}

func TestMissingFiles_NothingDownloaded(t *testing.T) {
	m, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	missing, err := localembed.MissingFiles(t.TempDir(), m)
	if err != nil {
		t.Fatalf("MissingFiles: %v", err)
	}
	if len(missing) != len(m.Files) {
		t.Errorf("MissingFiles reported %d of %d files missing, want all", len(missing), len(m.Files))
	}
	ok, err := localembed.IsDownloaded(t.TempDir(), m)
	if err != nil {
		t.Fatalf("IsDownloaded: %v", err)
	}
	if ok {
		t.Error("IsDownloaded = true for an empty directory")
	}
}

func TestMissingFiles_PartialAndComplete(t *testing.T) {
	m, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	root := t.TempDir()
	snapshot, err := localembed.SnapshotDir(root, m)
	if err != nil {
		t.Fatalf("SnapshotDir: %v", err)
	}

	// Write all but the last manifest entry, at their pinned sizes.
	for _, f := range m.Files[:len(m.Files)-1] {
		writeSized(t, filepath.Join(snapshot, filepath.FromSlash(f.Path)), f.Size)
	}
	last := m.Files[len(m.Files)-1]

	missing, err := localembed.MissingFiles(root, m)
	if err != nil {
		t.Fatalf("MissingFiles: %v", err)
	}
	if !slices.Equal(missing, []string{last.Path}) {
		t.Errorf("MissingFiles = %v, want [%s]", missing, last.Path)
	}

	writeSized(t, filepath.Join(snapshot, filepath.FromSlash(last.Path)), last.Size)
	ok, err := localembed.IsDownloaded(root, m)
	if err != nil {
		t.Fatalf("IsDownloaded: %v", err)
	}
	if !ok {
		t.Error("IsDownloaded = false with the full manifest present")
	}
}

// TestMissingFiles_WrongSizeCountsAsMissing means a truncated download is
// reported as missing rather than loaded and failing deeper in GoMLX.
func TestMissingFiles_WrongSizeCountsAsMissing(t *testing.T) {
	m, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	root := t.TempDir()
	snapshot, err := localembed.SnapshotDir(root, m)
	if err != nil {
		t.Fatalf("SnapshotDir: %v", err)
	}
	for _, f := range m.Files {
		writeSized(t, filepath.Join(snapshot, filepath.FromSlash(f.Path)), f.Size)
	}
	// Truncate one file.
	truncated := filepath.Join(snapshot, filepath.FromSlash(m.Files[0].Path))
	if err := os.WriteFile(truncated, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	missing, err := localembed.MissingFiles(root, m)
	if err != nil {
		t.Fatalf("MissingFiles: %v", err)
	}
	if !slices.Contains(missing, m.Files[0].Path) {
		t.Errorf("MissingFiles = %v, want it to include the truncated %s", missing, m.Files[0].Path)
	}
}

// TestMissingFiles_DanglingSymlink covers go-huggingface's real layout, where
// snapshot entries are symlinks into blobs/. A dangling link must count as
// missing, which is why the implementation uses Stat rather than Lstat.
func TestMissingFiles_DanglingSymlink(t *testing.T) {
	m, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	root := t.TempDir()
	snapshot, err := localembed.SnapshotDir(root, m)
	if err != nil {
		t.Fatalf("SnapshotDir: %v", err)
	}
	for _, f := range m.Files {
		writeSized(t, filepath.Join(snapshot, filepath.FromSlash(f.Path)), f.Size)
	}
	broken := filepath.Join(snapshot, filepath.FromSlash(m.Files[0].Path))
	if err := os.Remove(broken); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), broken); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	missing, err := localembed.MissingFiles(root, m)
	if err != nil {
		t.Fatalf("MissingFiles: %v", err)
	}
	if !slices.Contains(missing, m.Files[0].Path) {
		t.Errorf("MissingFiles = %v, want it to include the dangling %s", missing, m.Files[0].Path)
	}
}

// writeSized creates a file of exactly size bytes, so size checks can be
// exercised without materializing real weights.
func writeSized(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create %s: %v", path, err)
	}
	defer f.Close()
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			t.Fatalf("Truncate %s: %v", path, err)
		}
	}
}
