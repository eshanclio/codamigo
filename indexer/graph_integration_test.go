package indexer_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ieshan/go-code-chunker/chunker"
	"github.com/ieshan/go-code-chunker/langs"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/indexer"
	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/codamigo/walker"
)

// graphFixture indexes src as main.go and returns the store for inspection.
func graphFixture(t *testing.T, src string, opts ...indexer.Option) store.Store {
	t.Helper()
	ctx := t.Context()

	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	root := t.TempDir()
	if err = os.WriteFile(filepath.Join(root, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	w, err := walker.New(root, &config.Config{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() }) //nolint:errcheck

	idx := indexer.New(c, &fakeEmbedder{dim: dim}, s, w, opts...)
	if err = idx.Index(ctx); err != nil {
		t.Fatal(err)
	}
	return s
}

const graphSrc = `package main

import (
	"fmt"
	str "strings"
)

func Greet() {
	fmt.Println(helper())
	str.ToUpper("x")
}

func helper() string { return "hi" }
`

// Indexing a real file must land call edges and imports in the store, with each
// edge anchored to a chunk that actually exists.
func TestIndex_WritesGraph(t *testing.T) {
	s := graphFixture(t, graphSrc)
	ctx := t.Context()

	count, err := s.EdgeCount(ctx)
	if err != nil {
		t.Fatalf("EdgeCount: %v", err)
	}
	if count == 0 {
		t.Fatal("expected the index to contain edges")
	}

	// helper() is called from Greet, so it must be discoverable by target name.
	callers, err := s.EdgesByTargetName(ctx, "helper")
	if err != nil {
		t.Fatalf("EdgesByTargetName: %v", err)
	}
	if len(callers) == 0 {
		t.Error("expected an edge targeting helper")
	}

	// Every edge must anchor to a real chunk.
	symbols, err := s.ListSymbols(ctx)
	if err != nil {
		t.Fatalf("ListSymbols: %v", err)
	}
	known := make(map[string]struct{}, len(symbols))
	for _, sym := range symbols {
		known[sym.ID] = struct{}{}
	}
	for _, e := range callers {
		if _, ok := known[e.SrcID]; !ok {
			t.Errorf("edge %+v has no corresponding chunk", e)
		}
	}
}

func TestIndex_WritesImportsWithAlias(t *testing.T) {
	s := graphFixture(t, graphSrc)
	ctx := t.Context()

	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	imports, err := s.ImportsByFile(ctx, files)
	if err != nil {
		t.Fatalf("ImportsByFile: %v", err)
	}

	byModule := make(map[string]store.Import, len(imports))
	for _, im := range imports {
		byModule[im.Module] = im
	}
	if _, ok := byModule["fmt"]; !ok {
		t.Errorf("expected an fmt import, got %+v", imports)
	}
	if got, ok := byModule["strings"]; !ok {
		t.Errorf("expected a strings import, got %+v", imports)
	} else if got.Alias != "str" {
		t.Errorf("expected alias str, got %q", got.Alias)
	}
}

// WithGraph(false) must skip edge writes while still indexing chunks.
func TestIndex_GraphDisabled(t *testing.T) {
	s := graphFixture(t, graphSrc, indexer.WithGraph(false))
	ctx := t.Context()

	count, err := s.EdgeCount(ctx)
	if err != nil {
		t.Fatalf("EdgeCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no edges when graph is disabled, got %d", count)
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.ChunkCount == 0 {
		t.Error("chunks should still be indexed when the graph is disabled")
	}
}

// Re-indexing an edited file must replace its edges rather than accumulate them.
func TestIndex_ReindexReplacesGraph(t *testing.T) {
	ctx := t.Context()
	dim := 3
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", dim)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err = os.WriteFile(path, []byte(graphSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	w, err := walker.New(root, &config.Config{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck
	idx := indexer.New(c, &fakeEmbedder{dim: dim}, s, w)

	if err = idx.Index(ctx); err != nil {
		t.Fatal(err)
	}
	if edges, err := s.EdgesByTargetName(ctx, "helper"); err != nil {
		t.Fatal(err)
	} else if len(edges) == 0 {
		t.Fatal("expected an edge to helper before the edit")
	}

	// Rewrite the file so helper() is no longer called.
	edited := `package main

func Greet() {
}
`
	if err = os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = idx.Index(ctx); err != nil {
		t.Fatal(err)
	}

	edges, err := s.EdgesByTargetName(ctx, "helper")
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 0 {
		t.Errorf("stale edges survived re-index: %+v", edges)
	}
}
