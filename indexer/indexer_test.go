package indexer_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/ieshan/codamigo/chunker"
	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/indexer"
	"github.com/ieshan/codamigo/langs"
	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/codamigo/walker"
)

type fakeEmbedder struct {
	dim int
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	v := make([]float32, f.dim)
	if len(text) > 0 {
		v[0] = float32(len(text)) / 100.0
	}
	return v, nil
}

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i, t := range texts {
		v, _ := f.Embed(context.Background(), t)
		result[i] = v
	}
	return result, nil
}

func (f *fakeEmbedder) EmbedBatchPartial(ctx context.Context, texts []string) ([][]float32, []error) {
	vectors, err := f.EmbedBatch(ctx, texts)
	errs := make([]error, len(texts))
	if err != nil {
		for i := range errs {
			errs[i] = err
		}
	}
	return vectors, errs
}

func TestNew(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	root := t.TempDir()
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	emb := &fakeEmbedder{dim: dim}

	idx := indexer.New(nil, emb, s, w, 1, 0, 0, nil, nil)
	if idx == nil {
		t.Fatal("New returned nil")
	}
}

func TestIndexFiles_DeletesMissing(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	// Pre-insert a record for a file that doesn't exist on disk.
	rec := store.Record{
		ID: "r1", FilePath: "/nonexistent/file.go", Language: "go",
		Content: "package main", ContentHash: "h1", NodeKind: "file",
		Name: "file", StartLine: 1, EndLine: 1,
		Embedding: []float32{1, 0, 0},
	}
	if err = s.Upsert(ctx, []store.Record{rec}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: "/nonexistent/file.go", FileHash: "h1"}}); err != nil {
		t.Fatalf("set file hash: %v", err)
	}

	root := t.TempDir()
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	emb := &fakeEmbedder{dim: dim}
	idx := indexer.New(nil, emb, s, w, 1, 0, 0, nil, nil)

	// IndexFiles on a nonexistent path should trigger DeleteByFile.
	err = idx.IndexFiles(ctx, []string{"/nonexistent/file.go"})
	if err != nil {
		t.Fatalf("IndexFiles: %v", err)
	}

	// Verify the file was deleted from the store.
	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	for _, f := range files {
		if f == "/nonexistent/file.go" {
			t.Error("expected /nonexistent/file.go to be deleted from store")
		}
	}
}

type progressSpy struct {
	mu        sync.Mutex
	started   []string
	processed []string
	skipped   []string
}

func (s *progressSpy) FileStarted(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = append(s.started, path)
}

func (s *progressSpy) FileProcessed(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processed = append(s.processed, path)
}

func (s *progressSpy) FileSkipped(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skipped = append(s.skipped, path)
}

func (s *progressSpy) FileFailed(string, error) {}

func (s *progressSpy) Started() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.started)
}

func (s *progressSpy) Processed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.processed)
}

func (s *progressSpy) Skipped() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.skipped)
}

type failingEmbedder struct {
	dim       int
	failAfter int // 0 or negative = always fail; positive N = succeed first N EmbedBatch calls, then fail
	calls     atomic.Int32
}

func (f *failingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, fmt.Errorf("embedding API unavailable")
}

func (f *failingEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	n := int(f.calls.Add(1))
	if f.failAfter <= 0 || n > f.failAfter {
		return nil, fmt.Errorf("embedding API unavailable")
	}
	result := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, f.dim)
		if len(t) > 0 {
			v[0] = float32(len(t)) / 100.0
		}
		result[i] = v
	}
	return result, nil
}

func (f *failingEmbedder) EmbedBatchPartial(ctx context.Context, texts []string) ([][]float32, []error) {
	vectors, err := f.EmbedBatch(ctx, texts)
	errs := make([]error, len(texts))
	if err != nil {
		if vectors == nil {
			vectors = make([][]float32, len(texts))
		}
		for i := range errs {
			errs[i] = err
		}
	}
	return vectors, errs
}

// partialEmbedder succeeds for all texts EXCEPT those whose content matches
// any string in failTexts. Used to test per-chunk failure isolation.
type partialEmbedder struct {
	dim       int
	failTexts map[string]bool
}

func (p *partialEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return make([]float32, p.dim), nil
}

func (p *partialEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	vectors, errs := p.EmbedBatchPartial(ctx, texts)
	if joined := errors.Join(errs...); joined != nil {
		return nil, joined
	}
	return vectors, nil
}

func (p *partialEmbedder) EmbedBatchPartial(_ context.Context, texts []string) ([][]float32, []error) {
	vectors := make([][]float32, len(texts))
	errs := make([]error, len(texts))
	for i, t := range texts {
		fail := false
		for marker := range p.failTexts {
			if strings.Contains(t, marker) {
				fail = true
				break
			}
		}
		if fail {
			errs[i] = errors.New("simulated embed failure")
			continue
		}
		vectors[i] = make([]float32, p.dim)
		if len(t) > 0 {
			vectors[i][0] = float32(len(t)) / 100.0
		}
	}
	return vectors, errs
}

// recordingProgress captures Progress callbacks for assertion.
type recordingProgress struct {
	mu        sync.Mutex
	failed    map[string]error
	processed []string
	skipped   []string
}

func (r *recordingProgress) FileStarted(string) {}

func (r *recordingProgress) FileProcessed(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.processed = append(r.processed, path)
}

func (r *recordingProgress) FileSkipped(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skipped = append(r.skipped, path)
}

func (r *recordingProgress) FileFailed(path string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed == nil {
		r.failed = map[string]error{}
	}
	r.failed[path] = err
}

func TestIndexFiles_EmbeddingFailurePreservesOldChunks(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	root := t.TempDir()
	initialContent := "package main\n\nfunc main() {}\n"
	goFile := filepath.Join(root, "main.go")
	if err = os.WriteFile(goFile, []byte(initialContent), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}

	// First: index successfully with a working embedder and real chunker.
	goodEmb := &fakeEmbedder{dim: dim}
	idx := indexer.New(c, goodEmb, s, w, 1, 0, 0, nil, nil)
	if err = idx.IndexFiles(ctx, []string{goFile}); err != nil {
		t.Fatalf("initial index: %v", err)
	}

	// Verify chunks were stored.
	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected file to be indexed")
	}
	oldHashes, err := s.FileHashes(ctx, []string{goFile})
	if err != nil {
		t.Fatalf("file hash: %v", err)
	}
	oldHash := oldHashes[goFile]
	if oldHash == "" {
		t.Fatal("expected non-empty file hash after initial index")
	}

	// Modify the file so the hash changes and re-indexing is triggered.
	newContent := "package main\n\nfunc main() { println(\"hi\") }\n"
	if err = os.WriteFile(goFile, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-index with a failing embedder. The chunker will produce chunks, but
	// EmbedBatch will fail — old chunks should survive because ReplaceByFile
	// is never reached. IndexFiles logs per-file errors but does not return
	// them (error isolation for concurrent processing).
	badEmb := &failingEmbedder{dim: dim}
	idx2 := indexer.New(c, badEmb, s, w, 1, 0, 0, nil, nil)
	_ = idx2.IndexFiles(ctx, []string{goFile})

	// Verify old chunks are still intact (embedding failed before ReplaceByFiles).
	afterHashes, err := s.FileHashes(ctx, []string{goFile})
	if err != nil {
		t.Fatalf("file hash after failure: %v", err)
	}
	hash := afterHashes[goFile]
	if hash != oldHash {
		t.Errorf("file hash should still be %q after embedding failure, got %q", oldHash, hash)
	}
}

func TestIndexFiles_SkipsExcluded(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	root := t.TempDir()
	// Create a .md file (excluded by pattern)
	mdFile := filepath.Join(root, "README.md")
	if err = os.WriteFile(mdFile, []byte("# readme"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		ProjectRoot:     root,
		ExcludePatterns: []string{"*.md"},
	}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	emb := &fakeEmbedder{dim: dim}
	idx := indexer.New(nil, emb, s, w, 1, 0, 0, nil, nil)

	// IndexFiles on an excluded file should be a no-op (no chunker, nil chunker would error if called).
	err = idx.IndexFiles(ctx, []string{mdFile})
	if err != nil {
		t.Fatalf("IndexFiles on excluded file should not error: %v", err)
	}
}

func TestIndexFile_SkipsOutsideRoot(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	root := t.TempDir()
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	emb := &fakeEmbedder{dim: dim}
	idx := indexer.New(nil, emb, s, w, 1, 0, 0, nil, nil)

	// Create a file outside the project root.
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "outside.go")
	if err = os.WriteFile(outsidePath, []byte("package main"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err = idx.IndexFiles(ctx, []string{outsidePath}); err != nil {
		t.Errorf("IndexFiles should skip outside-root file, got error: %v", err)
	}

	// Store should remain empty — the outside file was silently skipped.
	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected store to be empty, got %d file(s): %v", len(files), files)
	}
}

// TestIndexFile_SkipsOutsideRoot_DotDotHidden verifies that a file whose name
// starts with ".." (e.g. "..hidden") at the project root is NOT falsely rejected
// by the path-traversal guard.
func TestIndexFile_DotDotHiddenNotRejected(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	root := t.TempDir()
	// A file named "..hidden" lives inside root — relPath will be "..hidden",
	// which must NOT be treated as a traversal path.
	dotDotFile := filepath.Join(root, "..hidden")
	if err = os.WriteFile(dotDotFile, []byte("package main"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	emb := &fakeEmbedder{dim: dim}
	idx := indexer.New(nil, emb, s, w, 1, 0, 0, nil, nil)

	if err = idx.IndexFiles(ctx, []string{dotDotFile}); err != nil {
		t.Errorf("IndexFiles returned unexpected error: %v", err)
	}

	// The file should have been processed (hash recorded), not skipped.
	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	if len(files) == 0 {
		t.Error("expected ..hidden file to be indexed, but store is empty")
	}
}

func TestIndex_ConcurrentProcessing(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	root := t.TempDir()
	for i := range 10 {
		content := fmt.Sprintf("package main\n\nfunc f%d() {}\n", i)
		os.WriteFile(filepath.Join(root, fmt.Sprintf("file%d.go", i)), []byte(content), 0o644) //nolint:errcheck
	}

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	emb := &fakeEmbedder{dim: dim}
	idx := indexer.New(nil, emb, s, w, 5, 0, 0, nil, nil)

	if err = idx.Index(ctx); err != nil {
		t.Fatalf("Index: %v", err)
	}

	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 10 {
		t.Errorf("expected 10 indexed files, got %d", len(files))
	}
}

func TestIndexer_Progress(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Run("reports processed on new file", func(t *testing.T) {
		ctx := t.Context()
		spy := &progressSpy{}
		dim := 3
		s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		root := t.TempDir()
		goFile := filepath.Join(root, "main.go")
		if err = os.WriteFile(goFile, []byte("package main"), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{ProjectRoot: root}
		w, err := walker.New(root, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close() //nolint:errcheck
		idx := indexer.New(c, &fakeEmbedder{dim: dim}, s, w, 1, 0, 0, nil, spy)
		if err = idx.Index(ctx); err != nil {
			t.Fatal(err)
		}
		if got := spy.Processed(); !slices.Equal(got, []string{goFile}) {
			t.Errorf("processed = %v; want [%s]", got, goFile)
		}
		if got := spy.Skipped(); len(got) != 0 {
			t.Errorf("skipped = %v; want empty", got)
		}
	})

	t.Run("reports skipped on hash match", func(t *testing.T) {
		ctx := t.Context()
		spy := &progressSpy{}
		dim := 3
		s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		root := t.TempDir()
		content := []byte("package main")
		goFile := filepath.Join(root, "main.go")
		if err = os.WriteFile(goFile, content, 0o644); err != nil {
			t.Fatal(err)
		}
		if err = s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: goFile, FileHash: store.ContentHash(content)}}); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{ProjectRoot: root}
		w, err := walker.New(root, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close() //nolint:errcheck
		idx := indexer.New(nil, &fakeEmbedder{dim: dim}, s, w, 1, 0, 0, nil, spy)
		if err = idx.Index(ctx); err != nil {
			t.Fatal(err)
		}
		if got := spy.Skipped(); len(got) != 1 {
			t.Errorf("skipped = %v; want one entry", got)
		}
		if got := spy.Processed(); len(got) != 0 {
			t.Errorf("processed = %v; want empty", got)
		}
	})

	t.Run("reports skipped on unsupported language", func(t *testing.T) {
		ctx := t.Context()
		spy := &progressSpy{}
		dim := 3
		s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		root := t.TempDir()
		txtFile := filepath.Join(root, "doc.txt")
		if err = os.WriteFile(txtFile, []byte("hello world"), 0o644); err != nil {
			t.Fatal(err)
		}
		c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{ProjectRoot: root}
		w, err := walker.New(root, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close() //nolint:errcheck
		idx := indexer.New(c, &fakeEmbedder{dim: dim}, s, w, 1, 0, 0, nil, spy)
		if err = idx.Index(ctx); err != nil {
			t.Fatal(err)
		}
		if got := spy.Skipped(); len(got) != 1 {
			t.Errorf("skipped = %v; want one entry for unsupported file", got)
		}
		if got := spy.Processed(); len(got) != 0 {
			t.Errorf("processed = %v; want empty", got)
		}
	})

	t.Run("reports skipped on oversized file", func(t *testing.T) {
		ctx := t.Context()
		spy := &progressSpy{}
		dim := 3
		s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		root := t.TempDir()
		bigFile := filepath.Join(root, "big.go")
		if err = os.WriteFile(bigFile, []byte(strings.Repeat("x", 100)), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{ProjectRoot: root}
		w, err := walker.New(root, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close() //nolint:errcheck
		idx := indexer.New(nil, &fakeEmbedder{dim: dim}, s, w, 1, 10, 0, nil, spy)
		if err = idx.Index(ctx); err != nil {
			t.Fatal(err)
		}
		if got := spy.Skipped(); len(got) != 1 {
			t.Errorf("skipped = %v; want one entry for oversized file", got)
		}
		if got := spy.Processed(); len(got) != 0 {
			t.Errorf("processed = %v; want empty", got)
		}
	})

	t.Run("nil progress does not panic", func(t *testing.T) {
		ctx := t.Context()
		dim := 3
		s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		root := t.TempDir()
		if err = os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{ProjectRoot: root}
		w, err := walker.New(root, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close() //nolint:errcheck
		idx := indexer.New(nil, &fakeEmbedder{dim: dim}, s, w, 1, 0, 0, nil, nil)
		if err = idx.Index(ctx); err != nil {
			t.Fatal(err)
		}
		// reaching here without panic is the assertion
	})
}

func TestIndexer_Progress_Concurrent(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const fileCount = 50
	spy := &progressSpy{}
	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	root := t.TempDir()
	for i := range fileCount {
		content := fmt.Sprintf("package main\n\nfunc f%d() {}\n", i)
		path := filepath.Join(root, fmt.Sprintf("file%d.go", i))
		if err = os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck
	idx := indexer.New(nil, &fakeEmbedder{dim: dim}, s, w, 8, 0, 0, nil, spy)

	synctest.Test(t, func(t *testing.T) {
		if err := idx.Index(t.Context()); err != nil {
			t.Fatal(err)
		}
		// Wait ensures all goroutines spawned within the bubble have completed
		// before we read the spy counts outside the bubble.
		synctest.Wait()
	})

	total := len(spy.Processed()) + len(spy.Skipped())
	if total != fileCount {
		t.Errorf("total progress calls = %d; want %d", total, fileCount)
	}
}

func TestIndexBatch_CrossFileEmbeddingReuse(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(":memory:", "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	root := t.TempDir()
	// Two files with identical content — should produce identical embeddings.
	content := "package main\n\nfunc Shared() {}\n"
	if err = os.WriteFile(filepath.Join(root, "a.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "b.go"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	emb := &fakeEmbedder{dim: dim}
	idx := indexer.New(c, emb, s, w, 2, 0, 0, nil, nil)

	if err = idx.Index(ctx); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Verify both files are indexed.
	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Verify both have embeddings by querying their content hashes.
	contentHash := store.ContentHash([]byte("func Shared() {}"))
	cached, err := s.EmbeddingsByContentHash(ctx, []string{contentHash})
	if err != nil {
		t.Fatalf("EmbeddingsByContentHash: %v", err)
	}
	if len(cached) == 0 {
		t.Error("expected cached embedding for shared content hash")
	}
}

func TestIndexBatch_StageBatchBoundary(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(":memory:", "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	root := t.TempDir()
	const fileCount = 20
	for i := range fileCount {
		content := fmt.Sprintf("package main\n\nfunc f%d() {}\n", i)
		if err = os.WriteFile(filepath.Join(root, fmt.Sprintf("file%d.go", i)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	spy := &progressSpy{}
	// concurrency=2 → stageBatchSize=8, so 20 files span 3 stage batches (8+8+4).
	idx := indexer.New(c, &fakeEmbedder{dim: dim}, s, w, 2, 0, 0, nil, spy)

	if err = idx.Index(ctx); err != nil {
		t.Fatalf("Index: %v", err)
	}

	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != fileCount {
		t.Errorf("expected %d files indexed, got %d", fileCount, len(files))
	}

	started := spy.Started()
	if len(started) != fileCount {
		t.Errorf("FileStarted callbacks = %d; want %d", len(started), fileCount)
	}

	total := len(spy.Processed()) + len(spy.Skipped())
	if total != fileCount {
		t.Errorf("progress callbacks = %d; want %d", total, fileCount)
	}
}

func TestIndexBatch_AllChunksFail_SkipsEntireBatch(t *testing.T) {
	// This test verifies whole-batch failure (all chunks fail). Per-chunk isolation
	// within a stage batch is covered by TestIndex_PartialEmbedFailure_SkipsAffectedFilesOnly.
	dim := 3
	s, err := store.NewSQLiteStore(":memory:", "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	root := t.TempDir()
	// Create 16 files. With concurrency=2, stageBatchSize=8, this gives 2 stage batches.
	for i := range 16 {
		content := fmt.Sprintf("package main\n\nfunc g%d() {}\n", i)
		if err = os.WriteFile(filepath.Join(root, fmt.Sprintf("file%02d.go", i)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	// failAfter=1: first EmbedBatch call succeeds (stage batch 1), second fails (stage batch 2).
	// Under the partial-failure regime, embedding failures no longer abort the
	// run; the second stage batch's files are reported as failed and skipped,
	// but Index() returns nil and earlier files remain committed.
	emb := &failingEmbedder{dim: dim, failAfter: 1}
	idx := indexer.New(c, emb, s, w, 2, 0, 0, nil, nil)

	if err = idx.Index(ctx); err != nil {
		t.Fatalf("Index returned unexpected error under partial-failure regime: %v", err)
	}

	// First stage batch (8 files) should be committed and searchable.
	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	// First stage batch (8 files) should be fully committed.
	if len(files) < 8 {
		t.Errorf("expected at least 8 files from the first stage batch, got %d", len(files))
	}
	if len(files) >= 16 {
		t.Errorf("expected second stage batch to have failed, but %d files indexed", len(files))
	}
}

// TestIndex_PartialEmbedFailure_SkipsAffectedFilesOnly verifies that when
// one file's chunks fail embedding, the OTHER file is still indexed and
// the failed file is reported via Progress.FileFailed.
func TestIndex_PartialEmbedFailure_SkipsAffectedFilesOnly(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer s.Close()

	root := t.TempDir()
	goodPath := filepath.Join(root, "good.go")
	badPath := filepath.Join(root, "bad.go")
	if err = os.WriteFile(goodPath, []byte("package main\n\nfunc Good() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(badPath, []byte("package main\n\nfunc BadChunkMarker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	emb := &partialEmbedder{
		dim:       dim,
		failTexts: map[string]bool{"BadChunkMarker": true},
	}
	prog := &recordingProgress{}

	idx := indexer.New(c, emb, s, w, 1, 0, 0, nil, prog)
	if err = idx.Index(t.Context()); err != nil {
		t.Fatalf("Index: %v", err)
	}

	goodHashes, err := s.FileHashes(t.Context(), []string{goodPath})
	if err != nil {
		t.Fatalf("FileHashes: %v", err)
	}
	if goodHashes[goodPath] == "" {
		t.Errorf("good.go was not indexed")
	}

	badHashes, err := s.FileHashes(t.Context(), []string{badPath})
	if err != nil {
		t.Fatalf("FileHashes: %v", err)
	}
	if badHashes[badPath] != "" {
		t.Errorf("bad.go should not have been written; got hash %q", badHashes[badPath])
	}

	if _, ok := prog.failed[badPath]; !ok {
		t.Errorf("Progress.FileFailed was not called for bad.go; got %v", prog.failed)
	}
}

// TestIndex_NilProgress_LogsButDoesNotPanic guards the MCP-watch path where
// the indexer is constructed with progress=nil. Partial failures must still
// be handled gracefully — no panic, surviving files written.
func TestIndex_NilProgress_LogsButDoesNotPanic(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer s.Close()

	root := t.TempDir()
	okPath := filepath.Join(root, "ok.go")
	failPath := filepath.Join(root, "fail.go")
	if err = os.WriteFile(okPath, []byte("package main\n\nfunc Ok() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(failPath, []byte("package main\n\nfunc BadChunkMarker() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	emb := &partialEmbedder{dim: dim, failTexts: map[string]bool{"BadChunkMarker": true}}

	idx := indexer.New(c, emb, s, w, 1, 0, 0, nil, nil) // progress=nil
	if err = idx.Index(context.Background()); err != nil {
		t.Fatalf("Index: %v", err)
	}

	okHashes, err := s.FileHashes(context.Background(), []string{okPath})
	if err != nil {
		t.Fatalf("FileHashes: %v", err)
	}
	if okHashes[okPath] == "" {
		t.Errorf("ok.go was not indexed under nil-progress path")
	}
}

// TestIndex_ContinuesAcrossStageBatches_AfterPartialFailure verifies the
// outer indexBatch loop does not abort when one stage batch has a partial
// failure — subsequent stage batches still run and their files are indexed.
func TestIndex_ContinuesAcrossStageBatches_AfterPartialFailure(t *testing.T) {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	defer s.Close()

	root := t.TempDir()
	const concurrency = 1
	// Files are created with names f0.txt..f8.txt. The walker produces
	// lexicographic order, so f0 lands in stage batch 1. This test relies
	// on that ordering being stable.
	// stageBatchSize = concurrency * 4 = 4. Create 9 files: stage 1 has 0..3,
	// stage 2 has 4..7, stage 3 has 8. Fail only file 0.
	for i := range 9 {
		name := filepath.Join(root, fmt.Sprintf("f%d.go", i))
		var content string
		if i == 0 {
			content = "package main\n\nfunc BadChunkMarker() {}\n"
		} else {
			content = fmt.Sprintf("package main\n\nfunc Ok%d() {}\n", i)
		}
		if err = os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close() //nolint:errcheck

	emb := &partialEmbedder{dim: dim, failTexts: map[string]bool{"BadChunkMarker": true}}

	idx := indexer.New(c, emb, s, w, concurrency, 0, 0, nil, nil)
	if err = idx.Index(context.Background()); err != nil {
		t.Fatalf("Index: %v", err)
	}

	// Files 1..8 should be indexed.
	for i := 1; i < 9; i++ {
		p := filepath.Join(root, fmt.Sprintf("f%d.go", i))
		h, err := s.FileHashes(context.Background(), []string{p})
		if err != nil {
			t.Fatalf("FileHashes: %v", err)
		}
		if h[p] == "" {
			t.Errorf("f%d.go not indexed — stage-batch loop aborted prematurely", i)
		}
	}
	// File 0 should NOT be indexed.
	p0 := filepath.Join(root, "f0.go")
	h, err := s.FileHashes(context.Background(), []string{p0})
	if err != nil {
		t.Fatalf("FileHashes: %v", err)
	}
	if h[p0] != "" {
		t.Errorf("f0.go should not have been written")
	}
}
