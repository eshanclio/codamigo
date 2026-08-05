package store_test

import (
	"testing"

	"github.com/ieshan/codamigo/store"
)

// graphStore opens an in-memory store for graph tests.
func graphStore(t *testing.T) store.Store {
	t.Helper()
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// graphRecord builds a minimal valid record for the 3-dimension test store.
func graphRecord(path, name string, startLine, endLine int) store.Record {
	content := "body of " + name
	return store.Record{
		ID:          store.RecordID(path, content),
		FilePath:    path,
		Language:    "go",
		Content:     content,
		ContentHash: store.ContentHash([]byte(content)),
		NodeKind:    "function_declaration",
		Name:        name,
		StartLine:   startLine,
		EndLine:     endLine,
		Embedding:   []float32{1, 0, 0},
	}
}

func TestGraph_EdgeRoundTrip(t *testing.T) {
	s := graphStore(t)
	ctx := t.Context()

	caller := graphRecord("a.go", "Caller", 1, 10)
	entry := store.FileRecords{
		FilePath: "a.go",
		FileHash: "hash-a",
		Records:  []store.Record{caller},
		Edges: []store.Edge{
			{SrcID: caller.ID, Kind: "call", DstName: "Helper", Line: 3},
			{SrcID: caller.ID, Kind: "call", DstName: "Println", DstQualifier: "fmt", Line: 4},
		},
		Imports: []store.Import{
			{Module: "fmt", Line: 2},
			{Module: "strings", Alias: "str", Line: 3},
		},
	}
	if err := s.ReplaceByFiles(ctx, []store.FileRecords{entry}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	edges, err := s.EdgesBySource(ctx, []string{caller.ID})
	if err != nil {
		t.Fatalf("EdgesBySource: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(edges))
	}
	if edges[0].DstName != "Helper" || edges[0].FilePath != "a.go" {
		t.Errorf("unexpected first edge: %+v", edges[0])
	}
	if edges[1].DstQualifier != "fmt" {
		t.Errorf("expected qualifier fmt, got %q", edges[1].DstQualifier)
	}

	byTarget, err := s.EdgesByTargetName(ctx, "Helper")
	if err != nil {
		t.Fatalf("EdgesByTargetName: %v", err)
	}
	if len(byTarget) != 1 || byTarget[0].SrcID != caller.ID {
		t.Errorf("expected one edge from Caller, got %+v", byTarget)
	}

	imports, err := s.ImportsByFile(ctx, []string{"a.go"})
	if err != nil {
		t.Fatalf("ImportsByFile: %v", err)
	}
	if len(imports) != 2 {
		t.Fatalf("expected 2 imports, got %d", len(imports))
	}
	if imports[1].Alias != "str" || imports[1].Module != "strings" {
		t.Errorf("expected aliased strings import, got %+v", imports[1])
	}

	count, err := s.EdgeCount(ctx)
	if err != nil {
		t.Fatalf("EdgeCount: %v", err)
	}
	if count != 2 {
		t.Errorf("EdgeCount = %d, want 2", count)
	}
}

// Re-indexing a file must replace its graph wholesale, leaving other files alone.
func TestGraph_ReplaceByFilesIsScopedToFile(t *testing.T) {
	s := graphStore(t)
	ctx := t.Context()

	a := graphRecord("a.go", "A", 1, 5)
	b := graphRecord("b.go", "B", 1, 5)
	if err := s.ReplaceByFiles(ctx, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1", Records: []store.Record{a},
			Edges:   []store.Edge{{SrcID: a.ID, Kind: "call", DstName: "Old", Line: 2}},
			Imports: []store.Import{{Module: "old/mod", Line: 1}},
		},
		{
			FilePath: "b.go", FileHash: "h2", Records: []store.Record{b},
			Edges:   []store.Edge{{SrcID: b.ID, Kind: "call", DstName: "Untouched", Line: 2}},
			Imports: []store.Import{{Module: "b/mod", Line: 1}},
		},
	}); err != nil {
		t.Fatalf("initial ReplaceByFiles: %v", err)
	}

	// Re-index a.go only, with a different edge set.
	a2 := graphRecord("a.go", "A2", 1, 6)
	if err := s.ReplaceByFiles(ctx, []store.FileRecords{{
		FilePath: "a.go", FileHash: "h1b", Records: []store.Record{a2},
		Edges:   []store.Edge{{SrcID: a2.ID, Kind: "call", DstName: "New", Line: 3}},
		Imports: []store.Import{{Module: "new/mod", Line: 1}},
	}}); err != nil {
		t.Fatalf("re-index ReplaceByFiles: %v", err)
	}

	if old, err := s.EdgesByTargetName(ctx, "Old"); err != nil {
		t.Fatalf("EdgesByTargetName(Old): %v", err)
	} else if len(old) != 0 {
		t.Errorf("stale edges survived re-index: %+v", old)
	}

	if fresh, err := s.EdgesByTargetName(ctx, "New"); err != nil {
		t.Fatalf("EdgesByTargetName(New): %v", err)
	} else if len(fresh) != 1 {
		t.Errorf("expected the new edge, got %+v", fresh)
	}

	if other, err := s.EdgesByTargetName(ctx, "Untouched"); err != nil {
		t.Fatalf("EdgesByTargetName(Untouched): %v", err)
	} else if len(other) != 1 {
		t.Errorf("another file's edges were disturbed: %+v", other)
	}

	imports, err := s.ImportsByFile(ctx, []string{"a.go"})
	if err != nil {
		t.Fatalf("ImportsByFile: %v", err)
	}
	if len(imports) != 1 || imports[0].Module != "new/mod" {
		t.Errorf("imports not replaced: %+v", imports)
	}
}

func TestGraph_DeleteByFileRemovesGraph(t *testing.T) {
	s := graphStore(t)
	ctx := t.Context()

	a := graphRecord("a.go", "A", 1, 5)
	if err := s.ReplaceByFiles(ctx, []store.FileRecords{{
		FilePath: "a.go", FileHash: "h1", Records: []store.Record{a},
		Edges:   []store.Edge{{SrcID: a.ID, Kind: "call", DstName: "Gone", Line: 2}},
		Imports: []store.Import{{Module: "some/mod", Line: 1}},
	}}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	if err := s.DeleteByFile(ctx, "a.go"); err != nil {
		t.Fatalf("DeleteByFile: %v", err)
	}

	count, err := s.EdgeCount(ctx)
	if err != nil {
		t.Fatalf("EdgeCount: %v", err)
	}
	if count != 0 {
		t.Errorf("edges survived DeleteByFile: %d remain", count)
	}
	imports, err := s.ImportsByFile(ctx, []string{"a.go"})
	if err != nil {
		t.Fatalf("ImportsByFile: %v", err)
	}
	if len(imports) != 0 {
		t.Errorf("imports survived DeleteByFile: %+v", imports)
	}
}

// A file with chunks but no edges (e.g. a language with no edge rules) is valid
// and must not error.
func TestGraph_NoEdgesIsValid(t *testing.T) {
	s := graphStore(t)
	ctx := t.Context()

	r := graphRecord("a.json", "config", 1, 3)
	if err := s.ReplaceByFiles(ctx, []store.FileRecords{{
		FilePath: "a.json", FileHash: "h", Records: []store.Record{r},
	}}); err != nil {
		t.Fatalf("ReplaceByFiles without edges: %v", err)
	}

	count, err := s.EdgeCount(ctx)
	if err != nil {
		t.Fatalf("EdgeCount: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no edges, got %d", count)
	}
}

func TestGraph_EmptyInputsAreNoOps(t *testing.T) {
	s := graphStore(t)
	ctx := t.Context()

	if edges, err := s.EdgesBySource(ctx, nil); err != nil || edges != nil {
		t.Errorf("EdgesBySource(nil) = %v, %v; want nil, nil", edges, err)
	}
	if edges, err := s.EdgesByTargetName(ctx, ""); err != nil || edges != nil {
		t.Errorf("EdgesByTargetName(\"\") = %v, %v; want nil, nil", edges, err)
	}
	if imports, err := s.ImportsByFile(ctx, nil); err != nil || imports != nil {
		t.Errorf("ImportsByFile(nil) = %v, %v; want nil, nil", imports, err)
	}
}

// ListSymbols must expose the chunk ID so resolution can map a name to the
// chunk that defines it.
func TestGraph_ListSymbolsCarriesChunkID(t *testing.T) {
	s := graphStore(t)
	ctx := t.Context()

	r := graphRecord("a.go", "Target", 1, 5)
	if err := s.ReplaceByFiles(ctx, []store.FileRecords{{
		FilePath: "a.go", FileHash: "h", Records: []store.Record{r},
	}}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	symbols, err := s.ListSymbols(ctx)
	if err != nil {
		t.Fatalf("ListSymbols: %v", err)
	}
	if len(symbols) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(symbols))
	}
	if symbols[0].ID != r.ID {
		t.Errorf("symbol ID = %q, want %q", symbols[0].ID, r.ID)
	}
}
