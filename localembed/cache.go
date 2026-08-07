package localembed

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// On-disk layout, one directory per model under the models root:
//
//	~/.codamigo/models/
//	└── bge-small-en-v1.5/                       ← ModelDir
//	    └── models--BAAI--bge-small-en-v1.5/     ← go-huggingface's own layout
//	        ├── blobs/  info/
//	        └── snapshots/<revision>/            ← SnapshotDir
//	            ├── model.safetensors  config.json  tokenizer.json
//	            └── modules.json       1_Pooling/config.json
//
// Everything below ModelDir belongs to go-huggingface; we only need to locate
// the snapshot to report what is present. Keeping each model under its own
// directory means removing one is `rm -rf <ModelDir>`.

// ModelDir returns the directory holding every file for m, given the models
// root. An empty root resolves to the caller's configured default, so callers
// must pass a non-empty root.
func ModelDir(root string, m Model) (string, error) {
	if root == "" {
		return "", errors.New("models root must not be empty")
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("models root %q must be an absolute path", root)
	}
	name := m.DirName()
	if err := safeDirName(name); err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// safeDirName rejects any model directory name that would resolve outside the
// models root. Lookup already constrains registry names and repository ids, so
// this is defence in depth for the path that actually touches the filesystem.
func safeDirName(name string) error {
	switch {
	case name == "":
		return errors.New("model directory name must not be empty")
	case name == "." || name == "..":
		return fmt.Errorf("model directory name %q is not allowed", name)
	case filepath.IsAbs(name):
		return fmt.Errorf("model directory name %q must not be absolute", name)
	case strings.ContainsRune(name, '/'), strings.ContainsRune(name, os.PathSeparator):
		return fmt.Errorf("model directory name %q must not contain a path separator", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("model directory name %q must not contain %q", name, "..")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("model directory name %q must not contain control characters", name)
		}
	}
	return nil
}

// flatRepoDir mirrors go-huggingface's repo_folder_name: the repository type and
// id joined by "--". Reimplemented rather than imported because the upstream
// helper is unexported, and we need it to find files without a network call.
func flatRepoDir(repoID string) string {
	return "models--" + strings.ReplaceAll(repoID, "/", "--")
}

// SnapshotDir returns the directory holding m's files inside modelDir.
//
// m.Revision must already be a concrete commit hash — [ResolvePin] is what
// guarantees that. Naming the path needs no filesystem access; whether the
// files are actually present is [MissingFiles]' question.
func SnapshotDir(modelDir string, m Model) (string, error) {
	if !isCommitHash(m.Revision) {
		return "", fmt.Errorf("%s has unresolved revision %q; call ResolvePin first",
			m.DisplayName(), m.Revision)
	}
	return filepath.Join(modelDir, flatRepoDir(m.RepoID), "snapshots", m.Revision), nil
}

// MissingFiles returns the manifest paths that are absent from modelDir or that
// have the wrong size, in manifest order. An empty result means the model is
// ready to load. Size is only checked for pinned entries.
//
// Files are reached through go-huggingface's snapshot symlinks, so os.Stat is
// used rather than os.Lstat: a dangling link must count as missing.
func MissingFiles(modelDir string, m Model) ([]string, error) {
	snapshot, err := SnapshotDir(modelDir, m)
	if err != nil {
		return nil, err
	}
	var missing []string
	for _, f := range m.Files {
		info, err := os.Stat(filepath.Join(snapshot, filepath.FromSlash(f.Path)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, f.Path)
		case err != nil:
			return nil, fmt.Errorf("checking %s: %w", f.Path, err)
		case f.Size > 0 && info.Size() != f.Size:
			missing = append(missing, f.Path)
		}
	}
	return missing, nil
}

// IsDownloaded reports whether every manifest file for m is present in
// modelDir. It does not verify checksums — that happens during [Download],
// where a mismatch can be acted on.
func IsDownloaded(modelDir string, m Model) (bool, error) {
	missing, err := MissingFiles(modelDir, m)
	if err != nil {
		return false, err
	}
	return len(missing) == 0, nil
}

// SnapshotInfo describes one snapshot directory on disk.
type SnapshotInfo struct {
	Path  string
	Bytes int64
}

// SupersededSnapshots returns the snapshot directories under modelDir other
// than keep, with their sizes.
//
// Moving an unpinned model to a newer upstream revision leaves the previous
// snapshot behind — often gigabytes of it. Reporting them is deliberate:
// deleting a working set of weights on the user's behalf is not this package's
// decision. A missing snapshots directory is not an error, just an empty
// result.
func SupersededSnapshots(modelDir string, m Model, keep string) ([]SnapshotInfo, error) {
	snapshots := filepath.Join(modelDir, flatRepoDir(m.RepoID), "snapshots")
	entries, err := os.ReadDir(snapshots)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", snapshots, err)
	}
	var out []SnapshotInfo
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		path := filepath.Join(snapshots, e.Name())
		out = append(out, SnapshotInfo{Path: path, Bytes: dirSize(path)})
	}
	return out, nil
}

// dirSize sums the sizes of the blobs a snapshot's entries point at. Errors are
// swallowed: this figure is reported to a human, never acted on.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry just does not count
		}
		// Snapshot entries are symlinks into blobs/, so follow them.
		if info, err := os.Stat(path); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
