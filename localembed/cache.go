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

// SnapshotDir returns the directory holding m's actual files inside modelDir.
//
// A pinned model names its revision, so the path is exact. An unpinned model
// tracks "main", whose commit hash is only known after the download, so the
// single directory under snapshots/ is used instead. Returns
// [ErrModelNotDownloaded] when there is nothing there yet.
func SnapshotDir(modelDir string, m Model) (string, error) {
	snapshots := filepath.Join(modelDir, flatRepoDir(m.RepoID), "snapshots")
	if m.Revision != "" && m.Revision != "main" {
		return filepath.Join(snapshots, m.Revision), nil
	}
	entries, err := os.ReadDir(snapshots)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s has no snapshot under %s", ErrModelNotDownloaded, m.DisplayName(), snapshots)
	}
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", snapshots, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	switch len(dirs) {
	case 0:
		return "", fmt.Errorf("%w: %s has no snapshot under %s", ErrModelNotDownloaded, m.DisplayName(), snapshots)
	case 1:
		return filepath.Join(snapshots, dirs[0]), nil
	default:
		// Several revisions of an unpinned model. Refuse rather than pick, since
		// silently loading the wrong weights is worse than an actionable error.
		return "", fmt.Errorf("%s has %d revisions under %s; pin embedding_model to a registry model "+
			"or remove the directory and re-download", m.DisplayName(), len(dirs), snapshots)
	}
}

// MissingFiles returns the manifest paths that are absent from modelDir or that
// have the wrong size, in manifest order. An empty result means the model is
// ready to load. Size is only checked for pinned entries.
//
// Files are reached through go-huggingface's snapshot symlinks, so os.Stat is
// used rather than os.Lstat: a dangling link must count as missing.
func MissingFiles(modelDir string, m Model) ([]string, error) {
	snapshot, err := SnapshotDir(modelDir, m)
	if errors.Is(err, ErrModelNotDownloaded) {
		paths := make([]string, len(m.Files))
		for i, f := range m.Files {
			paths[i] = f.Path
		}
		return paths, nil
	}
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
