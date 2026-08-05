package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/localembed"
	"github.com/ieshan/go-embedder"
)

// fakeEmbedder reports a fixed dimension; nothing here needs real vectors.
type fakeEmbedder struct{ dim int }

func (f fakeEmbedder) Embed(context.Context, string) ([]float32, error) {
	return make([]float32, f.dim), nil
}
func (f fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, f.dim)
	}
	return out, nil
}
func (f fakeEmbedder) EmbedBatchPartial(_ context.Context, texts []string) ([][]float32, []error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = make([]float32, f.dim)
	}
	return out, make([]error, len(texts))
}
func (f fakeEmbedder) Dim() int { return f.dim }

// TestStoreDim is the regression guard for the doctor bug this change fixes:
// doctor opened the store with cfg.EmbeddingDimensions (1536 by default) instead
// of the embedder's, so a 384-dimensional local model produced a spurious
// "[FAIL] Store open error".
func TestStoreDim_PrefersEmbedder(t *testing.T) {
	cfg := &config.Config{EmbeddingDimensions: 1536}
	if got := storeDim(fakeEmbedder{dim: 384}, cfg); got != 384 {
		t.Errorf("storeDim = %d, want 384 (the embedder's dimension wins)", got)
	}
}

func TestStoreDim_FallsBackToConfig(t *testing.T) {
	cfg := &config.Config{EmbeddingDimensions: 1536}
	if got := storeDim(nil, cfg); got != 1536 {
		t.Errorf("storeDim = %d, want 1536 when the embedder could not be built", got)
	}
}

// TestNewEmbedder_ReturnsUntypedNilOnError guards the non-nil-interface trap: if
// newEmbedder returned openai.New's typed nil directly, storeDim's `emb != nil`
// check would pass and Dim() would panic.
func TestNewEmbedder_ReturnsUntypedNilOnError(t *testing.T) {
	// An empty BaseURL makes openai.New fail.
	cfg := &config.Config{EmbeddingProvider: "openai", EmbeddingModel: "m"}
	emb, err := newEmbedder(cfg, roleQuery)
	if err == nil {
		t.Fatal("newEmbedder with no base URL = nil error, want error")
	}
	if emb != nil {
		t.Fatalf("newEmbedder returned a non-nil interface (%T) alongside an error", emb)
	}
	// The nil must survive the round trip through storeDim without panicking.
	if got := storeDim(emb, &config.Config{EmbeddingDimensions: 7}); got != 7 {
		t.Errorf("storeDim = %d, want 7", got)
	}
}

func TestNewEmbedder_NonLocalProvidersUseHTTPClient(t *testing.T) {
	// "voyage" is not special-cased anywhere, which is the whole point: adding a
	// provider name must not require a code change.
	for _, provider := range []string{"openai", "voyage", "some-future-provider"} {
		t.Run(provider, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.EmbeddingProvider = provider
			emb, err := newEmbedder(cfg, roleQuery)
			if err != nil {
				t.Fatalf("newEmbedder: %v", err)
			}
			defer closeEmbedder(emb)
			if _, ok := emb.(*localembed.Embedder); ok {
				t.Errorf("provider %q built the local embedder", provider)
			}
			if emb.Dim() != cfg.EmbeddingDimensions {
				t.Errorf("Dim() = %d, want %d", emb.Dim(), cfg.EmbeddingDimensions)
			}
		})
	}
}

// TestNewEmbedder_LocalRequiresDownloadedModel asserts the local path fails with
// the actionable error rather than silently downloading mid-index.
func TestNewEmbedder_LocalRequiresDownloadedModel(t *testing.T) {
	cfg := config.Defaults()
	cfg.EmbeddingProvider = localProvider
	cfg.EmbeddingModel = localembed.DefaultModel
	cfg.EmbeddingLocalModelDir = t.TempDir()

	emb, err := newEmbedder(cfg, roleDocument)
	if err == nil {
		closeEmbedder(emb)
		t.Fatal("newEmbedder with an empty models directory = nil error, want error")
	}
	if !errors.Is(err, localembed.ErrModelNotDownloaded) {
		t.Errorf("error = %v, want ErrModelNotDownloaded", err)
	}
	if emb != nil {
		t.Errorf("newEmbedder returned a non-nil interface (%T) alongside an error", emb)
	}
}

// TestNewEmbedder_QueryAndDocumentInputTypes: the role, not the input-type
// string, decides which side is built — both input types default to empty and
// would otherwise be indistinguishable.
func TestNewEmbedder_RoleSelectsInputType(t *testing.T) {
	cfg := config.Defaults()
	cfg.EmbeddingIndexInputType = "document"
	cfg.EmbeddingQueryInputType = "query"

	for _, role := range []embedderRole{roleDocument, roleQuery} {
		emb, err := newEmbedder(cfg, role)
		if err != nil {
			t.Fatalf("newEmbedder(role=%d): %v", role, err)
		}
		closeEmbedder(emb)
	}
}

func TestQueryEmbedderFor_HTTPProviderBuildsASecondClient(t *testing.T) {
	cfg := config.Defaults()
	document, err := newEmbedder(cfg, roleDocument)
	if err != nil {
		t.Fatalf("newEmbedder: %v", err)
	}
	defer closeEmbedder(document)

	query, err := queryEmbedderFor(cfg, document)
	if err != nil {
		t.Fatalf("queryEmbedderFor: %v", err)
	}
	defer closeEmbedder(query)
	if query == document {
		t.Error("queryEmbedderFor returned the same client; the input types would collide")
	}
}

func TestLocalModelsRoot(t *testing.T) {
	t.Run("configured override is made absolute", func(t *testing.T) {
		dir := t.TempDir()
		got, err := localModelsRoot(&config.Config{EmbeddingLocalModelDir: dir})
		if err != nil {
			t.Fatalf("localModelsRoot: %v", err)
		}
		if got != dir {
			t.Errorf("localModelsRoot = %q, want %q", got, dir)
		}
	})
	t.Run("default falls back to the models directory", func(t *testing.T) {
		got, err := localModelsRoot(&config.Config{})
		if err != nil {
			t.Fatalf("localModelsRoot: %v", err)
		}
		want, err := config.ModelsDir()
		if err != nil {
			t.Fatalf("ModelsDir: %v", err)
		}
		if got != want {
			t.Errorf("localModelsRoot = %q, want %q", got, want)
		}
	})
}

func TestCloseEmbedder_NoOpForNonClosers(t *testing.T) {
	// Must not panic for an embedder that holds no native resources.
	closeEmbedder(fakeEmbedder{dim: 4})
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{133466304, "127.3 MiB"},
		{5 << 30, "5.0 GiB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.bytes); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

// TestCommonFlags_HasProviderAndToken keeps the flag surface and flagsToConfig in
// step: a flag that exists but is never read into the config is silently inert,
// which is exactly what embedding_provider was before this change.
func TestCommonFlags_HasProviderAndToken(t *testing.T) {
	names := map[string]bool{}
	for _, f := range commonFlags {
		for _, n := range f.Names() {
			names[n] = true
		}
	}
	for _, want := range []string{"provider", "hf-token", "model", "api-key", "dimensions"} {
		if !names[want] {
			t.Errorf("commonFlags is missing --%s", want)
		}
	}
}

var _ embedder.Embedder = fakeEmbedder{}
