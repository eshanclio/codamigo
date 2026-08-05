package localembed_test

import (
	"bufio"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ieshan/go-embedder"

	"github.com/ieshan/codamigo/localembed"
)

// Compile-time assertions that the package satisfies the interfaces the wiring
// in cmd/codamigo relies on.
var (
	_ embedder.Embedder = (*localembed.Embedder)(nil)
	_ io.Closer         = (*localembed.Embedder)(nil)
)

// The tests below need real weights on disk. They skip rather than fail when the
// model is absent, so `make test` stays green on a clean checkout. Point
// CODAMIGO_TEST_MODELS_ROOT at a models directory to run them, or run
// `codamigo download-model` first.
// Takes testing.TB rather than *testing.T so BenchmarkEmbedBatch, which backs
// the published throughput table, shares the same skip logic.
func modelsRoot(tb testing.TB, model string) string {
	tb.Helper()
	root := os.Getenv("CODAMIGO_TEST_MODELS_ROOT")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			tb.Skipf("no home directory and CODAMIGO_TEST_MODELS_ROOT unset: %v", err)
		}
		root = filepath.Join(home, ".codamigo", "models")
	}
	m, err := localembed.Lookup(model)
	if err != nil {
		tb.Fatalf("Lookup(%q): %v", model, err)
	}
	dir, err := localembed.ModelDir(root, m)
	if err != nil {
		tb.Fatalf("ModelDir: %v", err)
	}
	ok, err := localembed.IsDownloaded(dir, m)
	if err != nil || !ok {
		tb.Skipf("model %s not downloaded under %s (run: codamigo download-model --model %s)", model, root, model)
	}
	return root
}

// newTestEmbedder builds an Embedder on the given backend, quieting the
// backend-selection log so test output stays readable.
func newTestEmbedder(tb testing.TB, model, backend string, opts ...func(*localembed.Options)) *localembed.Embedder {
	tb.Helper()
	if backend == "go" && raceEnabled {
		tb.Skip("the pure-Go backend trips checkptr inside GoMLX's matmul under -race; see race_test.go")
	}
	root := modelsRoot(tb, model)

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	tb.Cleanup(func() { slog.SetDefault(prev) })

	o := localembed.Options{Model: model, ModelsRoot: root, Backend: backend}
	for _, fn := range opts {
		fn(&o)
	}
	e, err := localembed.New(o)
	if err != nil {
		if errors.Is(err, localembed.ErrUnsupportedBackend) {
			tb.Skipf("backend %q unavailable: %v", backend, err)
		}
		tb.Fatalf("New: %v", err)
	}
	tb.Cleanup(func() {
		if err := e.Close(); err != nil {
			tb.Errorf("Close: %v", err)
		}
	})
	return e
}

// goldenPrompts are the four inputs whose reference embeddings are in
// testdata, produced by the Python sentence_transformers implementation. The
// first two are queries and carry the instruction prefix the reference used;
// the last two are documents and carry none.
var goldenPrompts = []struct {
	text   string
	prefix string
}{
	{"What is the capital of China?", "为这个句子生成表示以用于检索相关文章："},
	{"Explain gravity", "为这个句子生成表示以用于检索相关文章："},
	{"The capital of China is Beijing.", ""},
	{"Gravity is a force that attracts two bodies towards each other. It gives weight to physical objects and is responsible for the movement of planets around the sun.", ""},
}

// TestGoldenVectors is the one test that catches a pooling, normalization,
// tokenization, attention-mask, or prefix error. Each of those degrades
// retrieval quality silently, with no error anywhere, so nothing else in the
// suite would notice.
func TestGoldenVectors(t *testing.T) {
	for _, backend := range []string{"go", "xla"} {
		t.Run(backend, func(t *testing.T) {
			e := newTestEmbedder(t, "bge-small-en-v1.5", backend)
			want := loadGolden(t, e.Dim())

			for i, p := range goldenPrompts {
				emb := e
				if p.prefix != "" {
					emb = e.WithPrefix(p.prefix)
				}
				got, err := emb.Embed(t.Context(), p.text)
				if err != nil {
					t.Fatalf("prompt %d: Embed: %v", i, err)
				}
				if cos := cosine(got, want[i]); cos < 0.999 {
					t.Errorf("prompt %d: cosine to reference = %f, want >= 0.999", i, cos)
				}
			}
		})
	}
}

// TestGoldenVectors_PaddedBatch is the regression guard for deriving seqLen
// inside the graph. seqLen feeds the attention mask, so with a nil seqLen these
// same prompts still embed without error but drift to roughly 0.93 at 64 tokens
// and 0.87 at 128 — a silent quality loss. Batching prompts of unequal length
// forces padding, which is what makes the drift observable.
func TestGoldenVectors_PaddedBatch(t *testing.T) {
	e := newTestEmbedder(t, "bge-small-en-v1.5", "auto")
	want := loadGolden(t, e.Dim())

	// The two documents differ hugely in length, so the short one is heavily
	// padded within the batch.
	docs := []string{goldenPrompts[2].text, goldenPrompts[3].text}
	got, err := e.EmbedBatch(t.Context(), docs)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	for i := range docs {
		if cos := cosine(got[i], want[i+2]); cos < 0.999 {
			t.Errorf("document %d in a padded batch: cosine = %f, want >= 0.999 "+
				"(a value near 0.87-0.93 means seqLen is not reaching the attention mask)", i, cos)
		}
	}
}

// TestBackendParity proves the backend selector does not change numerics, so a
// fallback to the pure-Go backend is a speed decision and nothing more.
func TestBackendParity(t *testing.T) {
	goEmb := newTestEmbedder(t, "bge-small-en-v1.5", "go")
	xlaEmb := newTestEmbedder(t, "bge-small-en-v1.5", "xla")

	texts := []string{goldenPrompts[0].text, goldenPrompts[3].text, "func bucketize(lengths []int) {}"}
	goVecs, err := goEmb.EmbedBatch(t.Context(), texts)
	if err != nil {
		t.Fatalf("go EmbedBatch: %v", err)
	}
	xlaVecs, err := xlaEmb.EmbedBatch(t.Context(), texts)
	if err != nil {
		t.Fatalf("xla EmbedBatch: %v", err)
	}
	for i := range texts {
		if cos := cosine(goVecs[i], xlaVecs[i]); cos < 0.9999 {
			t.Errorf("text %d: go vs xla cosine = %f, want >= 0.9999", i, cos)
		}
	}
}

// TestEmbedBatchPartial_MixedValidity exercises the contract on a batch where
// some texts are rejected and others succeed — the case the indexer actually
// hits, and the one where an off-by-one in index mapping would silently pair a
// vector with the wrong chunk.
func TestEmbedBatchPartial_MixedValidity(t *testing.T) {
	e := newTestEmbedder(t, "bge-small-en-v1.5", "auto")

	texts := []string{"first real text", "   ", "second real text", "", "third real text"}
	vectors, errs := e.EmbedBatchPartial(t.Context(), texts)
	if len(vectors) != len(texts) || len(errs) != len(texts) {
		t.Fatalf("len(vectors)=%d len(errs)=%d, want both %d", len(vectors), len(errs), len(texts))
	}
	for i := range texts {
		valid := strings.TrimSpace(texts[i]) != ""
		if valid {
			if errs[i] != nil {
				t.Errorf("index %d: unexpected error %v", i, errs[i])
			}
			if len(vectors[i]) != e.Dim() {
				t.Errorf("index %d: vector has %d dims, want %d", i, len(vectors[i]), e.Dim())
			}
			continue
		}
		if errs[i] == nil {
			t.Errorf("index %d (%q) was accepted", i, texts[i])
		}
		if vectors[i] != nil {
			t.Errorf("index %d: rejected text got a vector", i)
		}
	}

	// The three valid texts are distinct, so their vectors must be too — this is
	// what catches a mapping that pairs a vector with the wrong index.
	if cosine(vectors[0], vectors[2]) > 0.999 {
		t.Error("distinct texts produced identical vectors; index mapping is suspect")
	}
}

// TestEmbedBatchPartial_LargeBatch crosses several batch and seq buckets at once
// and asserts every text comes back exactly once with a normalized vector.
func TestEmbedBatchPartial_LargeBatch(t *testing.T) {
	e := newTestEmbedder(t, "bge-small-en-v1.5", "auto")

	// 45 texts of varying length: spans the 1/8/32 batch ladder and several seq
	// buckets, and leaves a short final batch to be padded.
	texts := make([]string, 45)
	for i := range texts {
		texts[i] = strings.Repeat("token"+strconv.Itoa(i)+" ", 1+i*3)
	}
	vectors, errs := e.EmbedBatchPartial(t.Context(), texts)
	for i := range texts {
		if errs[i] != nil {
			t.Fatalf("index %d: %v", i, errs[i])
		}
		if len(vectors[i]) != e.Dim() {
			t.Fatalf("index %d: %d dims, want %d", i, len(vectors[i]), e.Dim())
		}
		// The pipeline ends in a Normalize module, so every vector is unit length.
		if norm := norm(vectors[i]); math.Abs(float64(norm)-1) > 1e-3 {
			t.Errorf("index %d: vector norm = %f, want 1", i, norm)
		}
	}
}

// TestQueryPrefix_AppliedToQueriesOnly is the structural half of the golden
// test: the registry's own prefix must change a query's vector and must not be
// applied by the document-side Embedder.
func TestQueryPrefix_AppliedToQueriesOnly(t *testing.T) {
	e := newTestEmbedder(t, "bge-small-en-v1.5", "auto")
	prefix := e.QueryPrefix()
	if prefix == "" {
		t.Fatal("QueryPrefix is empty for bge-small-en-v1.5")
	}
	q := e.WithPrefix(prefix)

	const text = "how does bucketing work"
	doc, err := e.Embed(t.Context(), text)
	if err != nil {
		t.Fatalf("document Embed: %v", err)
	}
	query, err := q.Embed(t.Context(), text)
	if err != nil {
		t.Fatalf("query Embed: %v", err)
	}
	if cosine(doc, query) > 0.9999 {
		t.Error("the query prefix had no effect on the vector")
	}

	// The document side must produce the same vector as an explicitly
	// unprefixed view, i.e. New really does return the unprefixed side.
	plain, err := e.WithPrefix("").Embed(t.Context(), text)
	if err != nil {
		t.Fatalf("unprefixed Embed: %v", err)
	}
	if cosine(doc, plain) < 0.9999 {
		t.Error("New's Embedder is applying a prefix to documents")
	}
}

// TestApplyQueryPrefix_OwnsClose is the regression guard for the query-side
// lifecycle. ApplyQueryPrefix must return the Embedder that owns Close, not a
// WithPrefix view: a view's Close is a no-op, so a caller that only needs the
// query side (search, map, callers, doctor, init) would defer a Close that never
// frees the compute backend. Asserted by requiring Close to actually take
// effect, which only the owner can do.
func TestApplyQueryPrefix_OwnsClose(t *testing.T) {
	root := modelsRoot(t, "bge-small-en-v1.5")
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e, err := localembed.New(localembed.Options{
		Model: "bge-small-en-v1.5", ModelsRoot: root, Backend: "auto",
		ApplyQueryPrefix: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The prefix is applied: the same text embeds differently from the
	// document-side Embedder built from the same weights.
	const text = "how does bucketing work"
	query, err := e.Embed(t.Context(), text)
	if err != nil {
		t.Fatalf("query Embed: %v", err)
	}
	doc, err := e.WithPrefix("").Embed(t.Context(), text)
	if err != nil {
		t.Fatalf("unprefixed Embed: %v", err)
	}
	if cosine(query, doc) > 0.9999 {
		t.Error("ApplyQueryPrefix did not apply the model's query prefix")
	}

	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := e.Embed(t.Context(), text); !errors.Is(err, localembed.ErrClosed) {
		t.Errorf("Embed after Close = %v, want ErrClosed; ApplyQueryPrefix "+
			"returned a non-owning view, so Close did nothing", err)
	}
}

// TestConcurrentEmbedding exercises the semaphore, the tokenizer lock, and the
// shared graph cache under -race.
func TestConcurrentEmbedding(t *testing.T) {
	e := newTestEmbedder(t, "bge-small-en-v1.5", "auto")
	query := e.WithPrefix(e.QueryPrefix())

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			emb := e
			if i%2 == 0 {
				emb = query // half the traffic goes through the shared view
			}
			texts := []string{
				"concurrent text " + strconv.Itoa(i),
				strings.Repeat("longer body ", 20+i*5),
			}
			vectors, errs := emb.EmbedBatchPartial(t.Context(), texts)
			for k := range texts {
				if errs[k] != nil {
					t.Errorf("goroutine %d index %d: %v", i, k, errs[k])
					continue
				}
				if len(vectors[k]) != e.Dim() {
					t.Errorf("goroutine %d index %d: %d dims", i, k, len(vectors[k]))
				}
			}
		})
	}
	wg.Wait()
}

// TestClose_Idempotent covers the double-Close and close-a-view cases, which is
// what makes `defer Close()` on both embedders in serve safe.
func TestClose_Idempotent(t *testing.T) {
	root := modelsRoot(t, "bge-small-en-v1.5")
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e, err := localembed.New(localembed.Options{
		Model: "bge-small-en-v1.5", ModelsRoot: root, Backend: "auto",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	view := e.WithPrefix("q: ")

	if err := e.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := view.Close(); err != nil {
		t.Errorf("Close on a view after the owner closed: %v", err)
	}

	// Calls through either handle must now be refused rather than touching
	// finalized buffers.
	for name, emb := range map[string]*localembed.Embedder{"owner": e, "view": view} {
		_, errs := emb.EmbedBatchPartial(t.Context(), []string{"text"})
		if !errors.Is(errs[0], localembed.ErrClosed) {
			t.Errorf("%s: EmbedBatchPartial after Close = %v, want ErrClosed", name, errs[0])
		}
	}
}

// TestClose_RacesInFlightCalls checks that Close waits for work already in
// flight instead of finalizing underneath it. Under -race, finalizing early
// would show up as a use-after-free or a crash in GoMLX.
func TestClose_RacesInFlightCalls(t *testing.T) {
	root := modelsRoot(t, "bge-small-en-v1.5")
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e, err := localembed.New(localembed.Options{
		Model: "bge-small-en-v1.5", ModelsRoot: root, Backend: "auto",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	texts := make([]string, 16)
	for i := range texts {
		texts[i] = strings.Repeat("body ", 30+i)
	}

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			// Either the call completes or it is refused; neither may crash.
			_, errs := e.EmbedBatchPartial(t.Context(), texts)
			for _, err := range errs {
				if err != nil && !errors.Is(err, localembed.ErrClosed) {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
	if err := e.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	wg.Wait()
}

func TestNew_MiniLMIsSymmetric(t *testing.T) {
	e := newTestEmbedder(t, "all-MiniLM-L6-v2", "auto")
	if e.Dim() != 384 {
		t.Errorf("Dim() = %d, want 384", e.Dim())
	}
	if e.QueryPrefix() != "" {
		t.Errorf("QueryPrefix() = %q, want empty for a symmetric model", e.QueryPrefix())
	}
	vec, err := e.Embed(t.Context(), "a probe sentence")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if math.Abs(float64(norm(vec))-1) > 1e-3 {
		t.Errorf("vector norm = %f, want 1", norm(vec))
	}
}

// TestTruncation asserts an over-long input is truncated rather than rejected,
// matching sentence_transformers.
func TestTruncation(t *testing.T) {
	e := newTestEmbedder(t, "bge-small-en-v1.5", "auto", func(o *localembed.Options) {
		o.MaxSeqLen = 128
	})
	if e.MaxSeqLen() != 128 {
		t.Errorf("MaxSeqLen() = %d, want 128", e.MaxSeqLen())
	}
	vec, err := e.Embed(t.Context(), strings.Repeat("word ", 5000))
	if err != nil {
		t.Fatalf("Embed on an over-long text: %v", err)
	}
	if len(vec) != e.Dim() {
		t.Errorf("vector has %d dims, want %d", len(vec), e.Dim())
	}
}

// loadGolden reads the reference embeddings: a '#'-prefixed comment naming each
// prompt, then one float per line. The upstream reader lives in an internal
// package, so it is reimplemented here.
func loadGolden(t *testing.T, dim int) [][]float32 {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "bge_small_en_v15_golden.txt"))
	if err != nil {
		t.Fatalf("opening golden fixture: %v", err)
	}
	defer f.Close()

	var out [][]float32
	var current []float32
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if current != nil {
				out = append(out, current)
			}
			current = make([]float32, 0, dim)
			continue
		}
		v, err := strconv.ParseFloat(line, 32)
		if err != nil {
			t.Fatalf("parsing golden float %q: %v", line, err)
		}
		current = append(current, float32(v))
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	if current != nil {
		out = append(out, current)
	}
	if len(out) != len(goldenPrompts) {
		t.Fatalf("golden fixture has %d vectors, want %d", len(out), len(goldenPrompts))
	}
	for i, v := range out {
		if len(v) != dim {
			t.Fatalf("golden vector %d has %d dims, want %d", i, len(v), dim)
		}
	}
	return out
}

func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

func norm(v []float32) float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	return float32(math.Sqrt(sum))
}
