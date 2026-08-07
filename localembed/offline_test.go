package localembed_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ieshan/codamigo/localembed"
)

// deadEndpoint returns the URL of a server that has already been shut down, so
// any attempt to reach it fails immediately rather than hanging.
func deadEndpoint(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

// TestNew_LoadsWithoutNetwork is the regression test for this whole feature.
//
// HF_ENDPOINT is read by hub.New, so pointing it at a closed port means any
// outbound HuggingFace request fails. New succeeding under that condition
// proves nothing but the local shim was consulted. Before the pin-and-shim
// change this failed with "config.json is missing or failed to download from
// repo" even though every file was already on disk.
func TestNew_LoadsWithoutNetwork(t *testing.T) {
	const model = "bge-small-en-v1.5"
	root := modelsRoot(t, model) // skips when the model is not downloaded
	t.Setenv("HF_ENDPOINT", deadEndpoint(t))

	emb, err := localembed.New(localembed.Options{
		Model:      model,
		ModelsRoot: root,
		Backend:    "go",
	})
	if err != nil {
		t.Fatalf("New with no reachable hub: %v", err)
	}
	defer func() { _ = emb.Close() }()

	// Loading is the point, but a real embedding proves the weights and
	// tokenizer resolved too, not just the config.
	vec, err := emb.Embed(t.Context(), "hello")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) != 384 {
		t.Errorf("Embed returned width %d, want 384 for %s", len(vec), model)
	}
}

func TestNew_UnpinnedWithoutPinIsActionable(t *testing.T) {
	// An unpinned repo id with nothing on disk must say what to run, not
	// produce an opaque HTTP error.
	t.Setenv("HF_ENDPOINT", deadEndpoint(t))
	_, err := localembed.New(localembed.Options{
		Model:      "org/not-downloaded",
		ModelsRoot: t.TempDir(),
		Dimensions: 8,
	})
	if err == nil {
		t.Fatal("New succeeded for a model that is not downloaded")
	}
	if !strings.Contains(err.Error(), "download-model") {
		t.Errorf("error %q does not tell the user how to recover", err)
	}
}

// TestNew_DamagedPinRepoInfoIsRejected is hermetic: it never touches
// ~/.codamigo/models.
//
// ReadPin only validates CommitHash, never RepoInfo. Without a guard, a pin
// with a valid hash but empty/mismatched RepoInfo would reach
// go-huggingface's DownloadInfo, whose LockedDownload unlinks info/<hash>
// before discovering the replacement is unusable — destroying the on-disk
// cache the offline path depends on. This asserts New refuses such a pin
// before anything can unlink that file, and that the pre-existing info file
// on disk is untouched afterward.
func TestNew_DamagedPinRepoInfoIsRejected(t *testing.T) {
	const testHash = "0123456789abcdef0123456789abcdef01234567"
	tests := []struct {
		name     string
		repoInfo json.RawMessage
	}{
		{"empty repo_info", nil},
		{"sha mismatch", json.RawMessage(`{"sha":"ffffffffffffffffffffffffffffffffffffffff","siblings":[]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			repoDir := filepath.Join(dir, "models--org--damaged")
			infoDir := filepath.Join(repoDir, "info")
			if err := os.MkdirAll(infoDir, 0o750); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}

			// A legitimate info file already on disk, as if left over from a
			// previous successful load. If the guard regresses, go-huggingface
			// would unlink and try to rewrite this before failing offline.
			goodInfo := `{"id":"org/damaged","sha":"` + testHash + `","siblings":[]}`
			infoPath := filepath.Join(infoDir, "main")
			if err := os.WriteFile(infoPath, []byte(goodInfo), 0o600); err != nil {
				t.Fatalf("WriteFile info: %v", err)
			}

			snapshot := filepath.Join(repoDir, "snapshots", testHash)
			if err := os.MkdirAll(filepath.Join(snapshot, "1_Pooling"), 0o750); err != nil {
				t.Fatalf("MkdirAll snapshot: %v", err)
			}
			for _, f := range []string{
				"config.json", "config_sentence_transformers.json", "modules.json",
				"tokenizer.json", "tokenizer_config.json", "1_Pooling/config.json", "model.safetensors",
			} {
				if err := os.WriteFile(filepath.Join(snapshot, f), []byte("{}"), 0o600); err != nil {
					t.Fatalf("WriteFile %s: %v", f, err)
				}
			}

			if err := localembed.WritePin(dir, localembed.Pin{
				RepoID:       "org/damaged",
				CommitHash:   testHash,
				ResolvedFrom: "main",
				RepoInfo:     tt.repoInfo,
			}); err != nil {
				t.Fatalf("WritePin: %v", err)
			}

			_, err := localembed.New(localembed.Options{
				Model:      "org/damaged",
				ModelsRoot: dir,
				Dimensions: 8,
			})
			if !errors.Is(err, localembed.ErrModelNotDownloaded) {
				t.Fatalf("New error = %v, want ErrModelNotDownloaded", err)
			}
			if !strings.Contains(err.Error(), "download-model") {
				t.Errorf("error %q does not tell the user how to recover", err)
			}

			got, err := os.ReadFile(infoPath)
			if err != nil {
				t.Fatalf("info file missing after New: %v", err)
			}
			if string(got) != goodInfo {
				t.Errorf("info file changed by a rejected load: got %q, want %q", got, goodInfo)
			}
		})
	}
}
