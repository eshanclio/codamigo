package localembed_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ieshan/codamigo/localembed"
)

const testHash = "0123456789abcdef0123456789abcdef01234567"

func TestPin_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := localembed.Pin{
		RepoID:       "org/model",
		CommitHash:   testHash,
		ResolvedFrom: "main",
		ResolvedAt:   time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		Dimensions:   768,
		RepoInfo:     json.RawMessage(`{"sha":"` + testHash + `","siblings":[]}`),
	}
	if err := localembed.WritePin(dir, want); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	got, err := localembed.ReadPin(dir)
	if err != nil {
		t.Fatalf("ReadPin: %v", err)
	}
	if got.CommitHash != want.CommitHash {
		t.Errorf("CommitHash = %q, want %q", got.CommitHash, want.CommitHash)
	}
	if got.RepoID != want.RepoID {
		t.Errorf("RepoID = %q, want %q", got.RepoID, want.RepoID)
	}
	if got.ResolvedFrom != want.ResolvedFrom {
		t.Errorf("ResolvedFrom = %q, want %q", got.ResolvedFrom, want.ResolvedFrom)
	}
	if got.Dimensions != want.Dimensions {
		t.Errorf("Dimensions = %d, want %d", got.Dimensions, want.Dimensions)
	}
	if !got.ResolvedAt.Equal(want.ResolvedAt) {
		t.Errorf("ResolvedAt = %v, want %v", got.ResolvedAt, want.ResolvedAt)
	}
	// RepoInfo must survive byte-for-byte: the shim serves it verbatim.
	if string(got.RepoInfo) != string(want.RepoInfo) {
		t.Errorf("RepoInfo = %s, want %s", got.RepoInfo, want.RepoInfo)
	}
}

func TestPin_WriteUsesRestrictivePermissions(t *testing.T) {
	dir := t.TempDir()
	p := localembed.Pin{RepoID: "org/model", CommitHash: testHash, RepoInfo: json.RawMessage(`{}`)}
	if err := localembed.WritePin(dir, p); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	info, err := os.Stat(localembed.PinPath(dir))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestPin_ReadAbsentIsErrNoPin(t *testing.T) {
	if _, err := localembed.ReadPin(t.TempDir()); !errors.Is(err, localembed.ErrNoPin) {
		t.Errorf("ReadPin on empty dir = %v, want ErrNoPin", err)
	}
}

func TestPin_ReadCorruptIsErrNoPin(t *testing.T) {
	// A truncated or garbage pin file must read as "no pin" so callers fall
	// through to derivation rather than failing outright.
	for name, body := range map[string]string{
		"not json":     "{{{",
		"empty object": "{}",
		"short hash":   `{"repo_id":"org/model","commit_hash":"abc","repo_info":{}}`,
		"non-hex hash": `{"repo_id":"org/model","commit_hash":"zzzz56789abcdef0123456789abcdef01234567","repo_info":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "codamigo-pin.json"), []byte(body), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := localembed.ReadPin(dir); !errors.Is(err, localembed.ErrNoPin) {
				t.Errorf("ReadPin = %v, want ErrNoPin", err)
			}
		})
	}
}

// writeDerivable lays out a model directory the way go-huggingface leaves one:
// an info file named after the requested revision, whose "sha" names an
// existing snapshot directory. No pin file.
func writeDerivable(t *testing.T, dir, requestedRev, sha string) {
	t.Helper()
	repoDir := filepath.Join(dir, "models--org--model")
	if err := os.MkdirAll(filepath.Join(repoDir, "info"), 0o750); err != nil {
		t.Fatalf("MkdirAll info: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "snapshots", sha), 0o750); err != nil {
		t.Fatalf("MkdirAll snapshots: %v", err)
	}
	body := `{"id":"org/model","sha":"` + sha + `","siblings":[]}`
	if err := os.WriteFile(filepath.Join(repoDir, "info", requestedRev), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile info: %v", err)
	}
}

func unpinnedTestModel() localembed.Model {
	return localembed.Model{
		RepoID:   "org/model",
		Revision: "main",
		Files:    []localembed.ManifestFile{{Path: "config.json"}},
	}
}

func registeredTestModel(revision string) localembed.Model {
	return localembed.Model{
		Name:       "test-model",
		RepoID:     "org/model",
		Revision:   revision,
		Registered: true,
		Files:      []localembed.ManifestFile{{Path: "config.json"}},
	}
}

func TestResolvePin_UnpinnedUsesPinFile(t *testing.T) {
	dir := t.TempDir()
	if err := localembed.WritePin(dir, localembed.Pin{
		RepoID: "org/model", CommitHash: testHash, ResolvedFrom: "main",
		RepoInfo: json.RawMessage(`{"sha":"` + testHash + `"}`),
	}); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	got, pin, err := localembed.ResolvePin(dir, unpinnedTestModel())
	if err != nil {
		t.Fatalf("ResolvePin: %v", err)
	}
	if got.Revision != testHash {
		t.Errorf("Revision = %q, want %q", got.Revision, testHash)
	}
	if pin.CommitHash != testHash {
		t.Errorf("pin.CommitHash = %q, want %q", pin.CommitHash, testHash)
	}
}

func TestResolvePin_UnpinnedDerivesWhenNoPinFile(t *testing.T) {
	// The migration path: an existing install has info/main and a snapshot but
	// no pin file, and must resolve with no network and no re-download.
	dir := t.TempDir()
	writeDerivable(t, dir, "main", testHash)

	got, pin, err := localembed.ResolvePin(dir, unpinnedTestModel())
	if err != nil {
		t.Fatalf("ResolvePin: %v", err)
	}
	if got.Revision != testHash {
		t.Errorf("Revision = %q, want %q", got.Revision, testHash)
	}
	if len(pin.RepoInfo) == 0 {
		t.Error("derived pin has empty RepoInfo; the shim would have nothing to serve")
	}
	// Derivation must not write anything: the load path stays read-only.
	if _, err := os.Stat(localembed.PinPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("derivation wrote a pin file; Stat err = %v, want not-exist", err)
	}
}

func TestResolvePin_DeriveRejectsShaWithoutSnapshot(t *testing.T) {
	// An info file claiming a revision we never actually downloaded is not a
	// usable pin.
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "models--org--model")
	if err := os.MkdirAll(filepath.Join(repoDir, "info"), 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := `{"sha":"` + testHash + `"}`
	if err := os.WriteFile(filepath.Join(repoDir, "info", "main"), []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := localembed.ResolvePin(dir, unpinnedTestModel()); !errors.Is(err, localembed.ErrModelNotDownloaded) {
		t.Errorf("ResolvePin = %v, want ErrModelNotDownloaded", err)
	}
}

func TestResolvePin_NothingResolvable(t *testing.T) {
	if _, _, err := localembed.ResolvePin(t.TempDir(), unpinnedTestModel()); !errors.Is(err, localembed.ErrModelNotDownloaded) {
		t.Errorf("ResolvePin on empty dir = %v, want ErrModelNotDownloaded", err)
	}
}

func TestResolvePin_RegistryRevisionWins(t *testing.T) {
	// A registry model's compiled-in revision is the supply-chain pin. Even a
	// well-formed pin file agreeing with it must not become the source of truth.
	dir := t.TempDir()
	if err := localembed.WritePin(dir, localembed.Pin{
		RepoID: "org/model", CommitHash: testHash, ResolvedFrom: testHash,
		RepoInfo: json.RawMessage(`{"sha":"` + testHash + `"}`),
	}); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	got, _, err := localembed.ResolvePin(dir, registeredTestModel(testHash))
	if err != nil {
		t.Fatalf("ResolvePin: %v", err)
	}
	if got.Revision != testHash {
		t.Errorf("Revision = %q, want the compiled-in %q", got.Revision, testHash)
	}
}

func TestResolvePin_RegistryMismatchIsRefused(t *testing.T) {
	const otherHash = "89abcdef0123456789abcdef0123456789abcdef"
	dir := t.TempDir()
	if err := localembed.WritePin(dir, localembed.Pin{
		RepoID: "org/model", CommitHash: otherHash, ResolvedFrom: "main",
		RepoInfo: json.RawMessage(`{"sha":"` + otherHash + `"}`),
	}); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	_, _, err := localembed.ResolvePin(dir, registeredTestModel(testHash))
	if err == nil {
		t.Fatal("ResolvePin succeeded with a pin that contradicts the compiled-in revision")
	}
	if !strings.Contains(err.Error(), "download-model") {
		t.Errorf("error %q does not tell the user how to recover", err)
	}
}
