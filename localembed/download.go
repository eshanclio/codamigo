package localembed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gomlx/go-huggingface/hub"
)

// DownloadOptions configures [Download].
type DownloadOptions struct {
	// Model is the model to fetch, from [Lookup].
	Model Model
	// ModelDir is the destination directory, from [ModelDir].
	ModelDir string
	// Token authenticates against HuggingFace. Optional: the registry models
	// download anonymously. Only needed for gated or private repositories.
	Token string
	// Endpoint overrides the HuggingFace base URL. Used by tests; empty means
	// the default (honouring HF_ENDPOINT as go-huggingface does).
	Endpoint string
	// Force re-downloads files that are already present and verified.
	Force bool
	// Progress enables go-huggingface's progress output on stdout.
	Progress bool
}

// DownloadResult reports what [Download] did.
type DownloadResult struct {
	// ModelDir is the directory that now holds the model.
	ModelDir string
	// Downloaded and Skipped list manifest paths, in manifest order.
	Downloaded []string
	Skipped    []string
	// Bytes is the total on-disk size of the manifest files.
	Bytes int64
	// Verified reports whether checksums were compared. False for an unpinned
	// repository id, where there is nothing to compare against.
	Verified bool
}

// Download fetches every manifest file for opts.Model into opts.ModelDir and,
// for a pinned model, verifies each file's SHA256 against the value recorded
// for its revision.
//
// Verification is a supply-chain check, not a corruption check: go-huggingface
// caches by ETag and never compares a content hash, and HuggingFace publishes
// real SHA256 sums only for LFS files — not for small ones like tokenizer.json.
// Comparing our own pinned constants against a pinned revision is what makes
// the download reproducible.
//
// Download is idempotent: a file already present with the right size and hash
// is skipped unless opts.Force is set. A file that fails verification is
// removed before the error is returned, so a retry starts clean.
func Download(ctx context.Context, opts DownloadOptions) (*DownloadResult, error) {
	m := opts.Model
	if opts.ModelDir == "" {
		return nil, errors.New("ModelDir must be set")
	}
	if len(m.Files) == 0 {
		return nil, fmt.Errorf("%w: %s has an empty manifest", ErrUnknownModel, m.DisplayName())
	}
	if err := os.MkdirAll(opts.ModelDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating model directory: %w", err)
	}

	repo := hub.New(m.RepoID).WithCacheDir(opts.ModelDir)
	if m.Revision != "" {
		repo = repo.WithRevision(m.Revision)
	}
	if opts.Token != "" {
		repo = repo.WithAuth(opts.Token)
	}
	if opts.Endpoint != "" {
		repo = repo.WithEndpoint(opts.Endpoint)
	}
	repo = repo.WithProgressBar(opts.Progress)
	if opts.Progress {
		repo.Verbosity = 1
	} else {
		repo.Verbosity = 0
	}
	// One file at a time: the manifest is ordered so the small metadata files
	// fail before the multi-megabyte weights start, and sequential progress
	// output is readable.
	repo.MaxParallelDownload = 1

	// DownloadInfo resolves the revision and the file list. It does not take a
	// context upstream, so Ctrl-C during this step is only noticed at the next
	// ctx check below.
	if err := repo.DownloadInfo(opts.Force); err != nil {
		return nil, downloadError(err, m, opts.Token)
	}

	result := &DownloadResult{ModelDir: opts.ModelDir, Verified: m.Pinned()}
	for _, f := range m.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path, downloaded, err := fetchFile(ctx, repo, opts, f)
		if err != nil {
			return nil, err
		}
		if downloaded {
			result.Downloaded = append(result.Downloaded, f.Path)
		} else {
			result.Skipped = append(result.Skipped, f.Path)
		}
		if info, err := os.Stat(path); err == nil {
			result.Bytes += info.Size()
		}
	}

	return result, nil
}

// fetchFile downloads one manifest file unless an acceptable copy is already
// present, and verifies it before returning. It reports whether a download
// actually happened so the caller can distinguish a no-op run.
func fetchFile(ctx context.Context, repo *hub.Repo, opts DownloadOptions, f ManifestFile) (path string, downloaded bool, err error) {
	if !opts.Force {
		if p, ok := existingFile(opts.ModelDir, opts.Model, f); ok {
			return p, false, nil
		}
	}
	path, err = repo.DownloadFileCtx(ctx, f.Path)
	if err != nil {
		// A cancelled context surfaces as a transport error upstream; report the
		// cause rather than a confusing download failure.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		if isAccessDenied(err) {
			return "", false, accessDeniedError(err, opts.Model, opts.Token)
		}
		return "", false, fmt.Errorf("downloading %s from %s: %w", f.Path, opts.Model.RepoID, err)
	}
	if err := verifyFile(path, f); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// existingFile reports whether f is already on disk and acceptable, so the
// download can be skipped. A wrong size or hash counts as absent: re-fetching
// is cheaper than making the user work out how to clear the cache by hand.
func existingFile(modelDir string, m Model, f ManifestFile) (string, bool) {
	snapshot, err := SnapshotDir(modelDir, m)
	if err != nil {
		return "", false
	}
	path := filepath.Join(snapshot, filepath.FromSlash(f.Path))
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}
	if f.Size > 0 && info.Size() != f.Size {
		return "", false
	}
	if f.SHA256 != "" {
		sum, err := sha256File(path)
		if err != nil || !strings.EqualFold(sum, f.SHA256) {
			return "", false
		}
	}
	return path, true
}

// verifyFile compares path against f's pinned hash and size, removing the file
// and the blob it links to on mismatch so a retry starts clean.
func verifyFile(path string, f ManifestFile) error {
	if f.SHA256 == "" && f.Size == 0 {
		return nil // unpinned: nothing to compare against
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", f.Path, err)
	}
	if f.Size > 0 && info.Size() != f.Size {
		removeDownloaded(path)
		return fmt.Errorf("%w for %s: got %d bytes, want %d", ErrChecksumMismatch, f.Path, info.Size(), f.Size)
	}
	if f.SHA256 == "" {
		return nil
	}
	sum, err := sha256File(path)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", f.Path, err)
	}
	if !strings.EqualFold(sum, f.SHA256) {
		removeDownloaded(path)
		return fmt.Errorf("%w for %s: got %s, want %s", ErrChecksumMismatch, f.Path, sum, f.SHA256)
	}
	return nil
}

// removeDownloaded deletes a snapshot entry and the blob it points at, so a
// rejected file is not silently reused on the next run. Errors are ignored:
// this runs on a failure path whose own error is the one worth reporting.
func removeDownloaded(path string) {
	if target, err := os.Readlink(path); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		_ = os.Remove(target)
	}
	_ = os.Remove(path)
}

func sha256File(path string) (string, error) {
	// #nosec G304 -- path is a file this process just downloaded into its own model cache dir, not external input
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }() // the file is only being read
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// downloadError annotates a repository-level failure.
func downloadError(err error, m Model, token string) error {
	if !isAccessDenied(err) {
		return fmt.Errorf("downloading %s: %w", m.RepoID, err)
	}
	return accessDeniedError(err, m, token)
}

// accessDeniedError adds the token hint only when no token was supplied. Told to
// set a token by a tool that was already given one, a user would go looking in
// the wrong place.
func accessDeniedError(err error, m Model, token string) error {
	if token != "" {
		return fmt.Errorf("%w for %s (the supplied token does not grant access): %w",
			ErrAccessDenied, m.RepoID, err)
	}
	return fmt.Errorf("%w for %s: it appears to be gated or private. "+
		"Set CODAMIGO_HF_TOKEN or HF_TOKEN, or embedding_hf_token in "+
		"~/.codamigo/global_settings.yml, then retry: %w", ErrAccessDenied, m.RepoID, err)
}

// isAccessDenied reports whether err looks like an HTTP 401 or 403.
//
// go-huggingface returns plain formatted strings rather than typed errors
// ("bad status code 403: ...", "failed with the following message: \"403
// Forbidden\""), so substring matching is the only option. It is deliberately
// additive: a false negative just means the caller sees the raw error, and a
// false positive adds a hint that is merely unhelpful.
func isAccessDenied(err error) bool {
	msg := err.Error()
	for _, needle := range []string{"401", "403", "Unauthorized", "Forbidden"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
