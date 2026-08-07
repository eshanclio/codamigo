package localembed

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// pinFileName is the pin file's name. It lives directly under ModelDir rather
// than inside the models--<id> tree, because everything below ModelDir belongs
// to go-huggingface (see the layout comment in cache.go).
const pinFileName = "codamigo-pin.json"

// Pin records the exact upstream revision a model directory holds, so loading
// the model needs no network round trip to discover it.
//
// RepoInfo is the verbatim RepoInfo JSON as it arrived from HuggingFace. It is
// kept byte-for-byte because [startInfoShim] serves it straight back to
// go-huggingface, which must be able to unmarshal it into its own hub.RepoInfo.
// Storing our own copy is not redundancy: go-huggingface's LockedDownload
// deletes its info/<revision> file before re-fetching it, so a shim reading
// that file would be reading something that had just been unlinked.
type Pin struct {
	RepoID string `json:"repo_id"`
	// CommitHash is the resolved 40-hex git commit the snapshot directory is
	// named after.
	CommitHash string `json:"commit_hash"`
	// ResolvedFrom is the revision that was asked for, "main" for an unpinned
	// model. Kept for diagnostics: it is what distinguishes "pinned upstream"
	// from "whatever main happened to be".
	ResolvedFrom string `json:"resolved_from"`
	// ResolvedAt is when download-model last consulted upstream.
	ResolvedAt time.Time `json:"resolved_at"`
	// Dimensions is the model's hidden size, recorded so download-model can
	// print a real embedding_dimensions value. Display only: it is never
	// consulted at load time.
	Dimensions int             `json:"dimensions,omitempty"`
	RepoInfo   json.RawMessage `json:"repo_info"`
}

// PinPath returns the pin file's path for a model directory.
func PinPath(modelDir string) string {
	return filepath.Join(modelDir, pinFileName)
}

// WritePin writes the pin file, replacing any existing one.
func WritePin(modelDir string, p Pin) error {
	if modelDir == "" {
		return fmt.Errorf("model directory must not be empty")
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encoding pin for %s: %w", p.RepoID, err)
	}
	if err := os.WriteFile(PinPath(modelDir), body, 0o600); err != nil {
		return fmt.Errorf("writing pin for %s: %w", p.RepoID, err)
	}
	return nil
}

// ReadPin reads the pin file.
//
// Absent, unreadable, unparseable and semantically unusable files all return
// [ErrNoPin] rather than distinct errors, because every caller treats them
// identically: fall through to deriving the revision from disk.
func ReadPin(modelDir string) (Pin, error) {
	// #nosec G304 -- modelDir is derived from the configured models root, not external input
	body, err := os.ReadFile(PinPath(modelDir))
	if err != nil {
		return Pin{}, fmt.Errorf("%w: %s: %w", ErrNoPin, PinPath(modelDir), err)
	}
	var p Pin
	if err := json.Unmarshal(body, &p); err != nil {
		return Pin{}, fmt.Errorf("%w: %s is not valid JSON: %w", ErrNoPin, PinPath(modelDir), err)
	}
	if !isCommitHash(p.CommitHash) {
		return Pin{}, fmt.Errorf("%w: %s has no valid commit_hash", ErrNoPin, PinPath(modelDir))
	}
	return p, nil
}

// isCommitHash reports whether s is a full 40-character hex git object id.
// Anything shorter is ambiguous and anything else is not a revision at all.
func isCommitHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
