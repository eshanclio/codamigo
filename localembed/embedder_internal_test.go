package localembed

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The validation and lifecycle paths below run before the inference graph is
// touched, which is why they can be exercised against a hand-built shared with
// no model, no backend, and no compiled graph. The code is ordered that way
// deliberately; if one of these tests starts needing a model, the ordering has
// regressed.
func stubEmbedder(t *testing.T) *Embedder {
	t.Helper()
	seqB, err := seqBuckets(512)
	if err != nil {
		t.Fatalf("seqBuckets: %v", err)
	}
	batchB, err := batchBuckets(32)
	if err != nil {
		t.Fatalf("batchBuckets: %v", err)
	}
	return &Embedder{
		owner: true,
		shared: &shared{
			descriptor: Model{Name: "stub", RepoID: "stub/stub"},
			dim:        8,
			maxSeqLen:  512,
			seqB:       seqB,
			batchB:     batchB,
		},
	}
}

func TestEmbedBatchPartial_EmptyInput(t *testing.T) {
	e := stubEmbedder(t)
	vectors, errs := e.EmbedBatchPartial(t.Context(), nil)
	if len(vectors) != 0 || len(errs) != 0 {
		t.Errorf("got %d vectors and %d errs, want 0 and 0", len(vectors), len(errs))
	}
}

func TestEmbedBatch_EmptyInput(t *testing.T) {
	e := stubEmbedder(t)
	vectors, err := e.EmbedBatch(t.Context(), nil)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vectors) != 0 {
		t.Errorf("got %d vectors, want 0", len(vectors))
	}
}

// TestEmbedBatchPartial_WhitespaceOnly asserts the contract holds on the
// rejection path: an errored index must have a nil vector, and vice versa.
func TestEmbedBatchPartial_WhitespaceOnly(t *testing.T) {
	e := stubEmbedder(t)
	texts := []string{"", "   ", "\t\n", " \r\n "}
	vectors, errs := e.EmbedBatchPartial(t.Context(), texts)
	assertContract(t, texts, vectors, errs)
	for i := range texts {
		if errs[i] == nil {
			t.Errorf("text %d (%q) was accepted, want a rejection", i, texts[i])
		}
	}
}

func TestEmbed_WhitespaceOnly(t *testing.T) {
	e := stubEmbedder(t)
	if _, err := e.Embed(t.Context(), "   "); err == nil {
		t.Error("Embed on whitespace = nil error, want error")
	}
}

func TestEmbedBatchPartial_AfterClose(t *testing.T) {
	e := stubEmbedder(t)
	// close() on the stub would finalize nil resources, so mark it closed the
	// same way close() does rather than calling it.
	e.shared.mu.Lock()
	e.shared.closed = true
	e.shared.mu.Unlock()

	texts := []string{"one", "two"}
	vectors, errs := e.EmbedBatchPartial(t.Context(), texts)
	assertContract(t, texts, vectors, errs)
	for i := range texts {
		if !errors.Is(errs[i], ErrClosed) {
			t.Errorf("errs[%d] = %v, want ErrClosed", i, errs[i])
		}
	}
}

func TestEmbedBatchPartial_CancelledContext(t *testing.T) {
	e := stubEmbedder(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	texts := []string{"one", "two"}
	vectors, errs := e.EmbedBatchPartial(ctx, texts)
	assertContract(t, texts, vectors, errs)
	for i := range texts {
		if errs[i] == nil {
			t.Errorf("errs[%d] = nil, want a cancellation error", i)
		}
	}
}

// TestClose_OnPrefixViewIsNoOp: only the Embedder from New owns the resources,
// so a deferred Close on either is safe.
func TestClose_OnPrefixViewIsNoOp(t *testing.T) {
	e := stubEmbedder(t)
	view := e.WithPrefix("prefix: ")
	if err := view.Close(); err != nil {
		t.Errorf("Close on a WithPrefix view = %v, want nil", err)
	}
	e.shared.mu.RLock()
	closed := e.shared.closed
	e.shared.mu.RUnlock()
	if closed {
		t.Error("Close on a view closed the owner")
	}
}

func TestWithPrefix_SharesState(t *testing.T) {
	e := stubEmbedder(t)
	view := e.WithPrefix("q: ")
	if view.shared != e.shared {
		t.Error("WithPrefix did not share the underlying model")
	}
	if view.prefix != "q: " {
		t.Errorf("view prefix = %q, want %q", view.prefix, "q: ")
	}
	if e.prefix != "" {
		t.Errorf("owner prefix = %q, want empty: documents must not be prefixed", e.prefix)
	}
	if view.owner {
		t.Error("view claims ownership")
	}
	if view.Dim() != e.Dim() {
		t.Error("view reports a different dimension")
	}
}

func TestCheckDimensions(t *testing.T) {
	registered := Model{Name: "reg", RepoID: "o/reg", Dimensions: 384, Registered: true}
	raw := Model{RepoID: "o/raw"}

	tests := []struct {
		name       string
		model      Model
		configured int
		actual     int
		wantErr    bool
	}{
		{"registry model agrees", registered, 0, 384, false},
		{"registry model ignores a stale config value", registered, 1536, 384, false},
		{"stale registry entry", Model{Name: "r", RepoID: "o/r", Dimensions: 768, Registered: true}, 0, 384, true},
		{"raw model with a matching config", raw, 384, 384, false},
		{"raw model with no config", raw, 0, 384, true},
		{"raw model with a wrong config", raw, 1536, 384, true},
		{"model reports nothing", registered, 384, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDimensions(tt.model, tt.configured, tt.actual)
			if tt.wantErr != (err != nil) {
				t.Fatalf("checkDimensions = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrDimensionMismatch) {
				t.Errorf("error should wrap ErrDimensionMismatch, got: %v", err)
			}
		})
	}
}

// TestCheckDimensions_RawModelNamesTheValue: the error has to tell the user what
// to put in embedding_dimensions, since that value decides the store's vector
// width and a wrong one makes the store unopenable.
func TestCheckDimensions_RawModelNamesTheValue(t *testing.T) {
	err := checkDimensions(Model{RepoID: "o/raw"}, 0, 384)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "384") || !strings.Contains(err.Error(), "embedding_dimensions") {
		t.Errorf("error should name embedding_dimensions and 384, got: %v", err)
	}
}

func TestResolveMaxSeqLen(t *testing.T) {
	tests := []struct {
		requested, modelMax, want int
	}{
		{0, 512, 512},      // unset falls back to the model's own maximum
		{128, 512, 128},    // honoured when within range
		{512, 512, 512},    // exactly at the maximum
		{1024, 512, 512},   // clamped down: position embeddings cannot go further
		{-1, 512, 512},     // nonsense falls back
		{256, 0, 256},      // model does not declare one; assume 512
		{0, 0, 512},        //
		{4096, 2048, 2048}, //
	}
	for _, tt := range tests {
		if got := resolveMaxSeqLen(tt.requested, tt.modelMax); got != tt.want {
			t.Errorf("resolveMaxSeqLen(%d, %d) = %d, want %d", tt.requested, tt.modelMax, got, tt.want)
		}
	}
}

func TestNew_RequiresModelsRoot(t *testing.T) {
	if _, err := New(Options{Model: DefaultModel}); err == nil {
		t.Error("New without ModelsRoot = nil error, want error")
	}
}

func TestNew_ModelNotDownloaded(t *testing.T) {
	_, err := New(Options{Model: DefaultModel, ModelsRoot: t.TempDir()})
	if !errors.Is(err, ErrModelNotDownloaded) {
		t.Fatalf("New with an empty models root = %v, want ErrModelNotDownloaded", err)
	}
	if !strings.Contains(err.Error(), "download-model") {
		t.Errorf("error should name the command that fixes it, got: %v", err)
	}
}

func TestNew_UnknownModel(t *testing.T) {
	_, err := New(Options{Model: "not-a-model", ModelsRoot: t.TempDir()})
	if !errors.Is(err, ErrUnknownModel) {
		t.Errorf("New with an unknown model = %v, want ErrUnknownModel", err)
	}
}

// assertContract checks embedder.Embedder's documented invariants. The indexer
// enforces these at its call site, so a violation here is a hard failure there.
func assertContract(t *testing.T, texts []string, vectors [][]float32, errs []error) {
	t.Helper()
	if len(vectors) != len(texts) || len(errs) != len(texts) {
		t.Fatalf("len(vectors)=%d, len(errs)=%d, want both %d", len(vectors), len(errs), len(texts))
	}
	for i := range texts {
		switch {
		case errs[i] == nil && vectors[i] == nil:
			t.Errorf("index %d: no error but a nil vector", i)
		case errs[i] != nil && vectors[i] != nil:
			t.Errorf("index %d: error %v but a non-nil vector", i, errs[i])
		}
	}
}
