package localembed_test

import (
	"net/http"
	"net/http/httptest"
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
