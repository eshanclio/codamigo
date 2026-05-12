package query_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ieshan/codamigo/query"
	"github.com/ieshan/codamigo/store"
)

type fakeEmbedder struct {
	vec []float32
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return f.vec, nil
}

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i := range texts {
		result[i] = f.vec
	}
	return result, nil
}

func TestQuerier_Search(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "main.go", Language: "go",
			Content: "func main() {}", ContentHash: "h1", NodeKind: "function",
			Name: "main", StartLine: 1, EndLine: 3,
			Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "helper.go", Language: "go",
			Content: "func helper() {}", ContentHash: "h2", NodeKind: "function",
			Name: "helper", StartLine: 1, EndLine: 5,
			Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	sr, err := q.Search(ctx, "main function", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(sr.Results) == 0 {
		t.Fatal("expected at least one result")
	}
	if sr.Results[0].FilePath != "main.go" {
		t.Errorf("expected first result to be main.go, got %s", sr.Results[0].FilePath)
	}
}

func TestQuerier_SearchWithOptions_PathFilter(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "src/main.go", Language: "go",
			Content: "func main() {}", ContentHash: "h1", NodeKind: "function",
			Name: "main", StartLine: 1, EndLine: 3,
			Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "test/main_test.go", Language: "go",
			Content: "func TestMain() {}", ContentHash: "h2", NodeKind: "function",
			Name: "TestMain", StartLine: 1, EndLine: 5,
			Embedding: []float32{0.9, 0.1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	sr, err := q.SearchWithOptions(ctx, "main", query.SearchOptions{
		Limit: 10,
		Paths: []string{"src/**"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range sr.Results {
		if r.FilePath == "test/main_test.go" {
			t.Error("path filter should exclude test/main_test.go")
		}
	}
	if len(sr.Results) != 1 || sr.Results[0].FilePath != "src/main.go" {
		t.Errorf("expected exactly [src/main.go], got %v", sr.Results)
	}
}

func TestQuerier_SearchWithOptions_Offset(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{ID: "r1", FilePath: "a.go", Language: "go", Content: "func a(){}", ContentHash: "h1", NodeKind: "fn", Name: "a", Embedding: []float32{1, 0, 0}},
		{ID: "r2", FilePath: "b.go", Language: "go", Content: "func b(){}", ContentHash: "h2", NodeKind: "fn", Name: "b", Embedding: []float32{0.9, 0.1, 0}},
		{ID: "r3", FilePath: "c.go", Language: "go", Content: "func c(){}", ContentHash: "h3", NodeKind: "fn", Name: "c", Embedding: []float32{0.8, 0.2, 0}},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	page0, err := q.SearchWithOptions(ctx, "func", query.SearchOptions{Limit: 2})
	if err != nil {
		t.Fatalf("page0: %v", err)
	}
	page1, err := q.SearchWithOptions(ctx, "func", query.SearchOptions{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}

	seen := map[string]bool{}
	for _, r := range page0.Results {
		seen[r.FilePath] = true
	}
	for _, r := range page1.Results {
		if seen[r.FilePath] {
			t.Errorf("file %q appeared in both pages", r.FilePath)
		}
	}
}

func TestPackageFromPath(t *testing.T) {
	tests := []struct {
		name        string
		projectRoot string
		filePath    string
		want        string
	}{
		{"top-level file", "/repo", "/repo/main.go", "."},
		{"single-level package", "/repo", "/repo/store/sqlite.go", "store"},
		{"nested package", "/repo", "/repo/embedder/openaicompat/client.go", "embedder/openaicompat"},
		{"cmd package", "/repo", "/repo/cmd/codamigo/main.go", "cmd/codamigo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := query.PackageFromPath(tt.projectRoot, tt.filePath)
			if got != tt.want {
				t.Errorf("PackageFromPath(%q, %q) = %q, want %q", tt.projectRoot, tt.filePath, got, tt.want)
			}
		})
	}
}

func TestQuerier_Map_BasicOutput(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/store/store.go", Language: "go",
			Content: "type Store interface{}", ContentHash: "h1",
			NodeKind: "interface_type", Name: "Store", Parent: "",
			StartLine: 10, EndLine: 20, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/store/store.go", Language: "go",
			Content: "func Search() {}", ContentHash: "h2",
			NodeKind: "function_declaration", Name: "Search", Parent: "Store",
			StartLine: 30, EndLine: 40, Embedding: []float32{0, 1, 0},
		},
		{
			ID: "r3", FilePath: "/repo/indexer/indexer.go", Language: "go",
			Content: "func Index() {}", ContentHash: "h3",
			NodeKind: "function_declaration", Name: "Index", Parent: "",
			StartLine: 5, EndLine: 15, Embedding: []float32{0, 0, 1},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if result == "" {
		t.Fatal("Map returned empty string")
	}

	storeIdx := strings.Index(result, "# package: store")
	indexerIdx := strings.Index(result, "# package: indexer")
	if storeIdx == -1 {
		t.Error("missing store package header")
	}
	if indexerIdx == -1 {
		t.Error("missing indexer package header")
	}
	if storeIdx > indexerIdx {
		t.Error("store package should appear before indexer (more symbols)")
	}

	if !strings.Contains(result, "interface Store") {
		t.Error("expected 'interface Store' in output")
	}
	if !strings.Contains(result, "func Search") {
		t.Error("expected 'func Search' in output")
	}
	if !strings.Contains(result, "func Index") {
		t.Error("expected 'func Index' in output")
	}
}

func TestQuerier_Map_Truncation(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var records []store.Record
	for i := range 20 {
		fp := fmt.Sprintf("/repo/pkg%d/file.go", i)
		records = append(records, store.Record{
			ID: fmt.Sprintf("r%d", i), FilePath: fp, Language: "go",
			Content:     fmt.Sprintf("func Func%d() { /* padding to make content larger */ }", i),
			ContentHash: fmt.Sprintf("h%d", i),
			NodeKind:    "function_declaration", Name: fmt.Sprintf("Func%d", i),
			StartLine: 1, EndLine: 10, Embedding: []float32{1, 0, 0},
		})
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 50})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result, "truncated") {
		t.Error("expected truncation trailer in output")
	}
}

func TestQuerier_Map_EmptyStore(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(context.Background(), query.MapOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty string for empty store, got %q", result)
	}
}

func TestQuerier_Map_NestedSymbolsIndented(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/store/store.go", Language: "go",
			Content: "type Store interface{}", ContentHash: "h1",
			NodeKind: "interface_type", Name: "Store", Parent: "",
			StartLine: 10, EndLine: 20, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/store/store.go", Language: "go",
			Content: "func Search() {}", ContentHash: "h2",
			NodeKind: "method_declaration", Name: "Search", Parent: "Store",
			StartLine: 30, EndLine: 40, Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}

	if !strings.Contains(result, "    func Search") {
		t.Errorf("expected nested symbol with 4-space indent, got:\n%s", result)
	}
	if !strings.Contains(result, "  interface Store") {
		t.Errorf("expected top-level symbol with 2-space indent, got:\n%s", result)
	}
}

func TestQuerier_SearchWithOptions_MaxTokens(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	content := strings.Repeat("x", 100)
	records := []store.Record{
		{ID: "r1", FilePath: "a.go", Language: "go", Content: content + "1", ContentHash: "h1", NodeKind: "fn", Name: "a", Embedding: []float32{1, 0, 0}},
		{ID: "r2", FilePath: "b.go", Language: "go", Content: content + "2", ContentHash: "h2", NodeKind: "fn", Name: "b", Embedding: []float32{0.9, 0.1, 0}},
		{ID: "r3", FilePath: "c.go", Language: "go", Content: content + "3", ContentHash: "h3", NodeKind: "fn", Name: "c", Embedding: []float32{0.8, 0.2, 0}},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	sr, err := q.SearchWithOptions(ctx, "test", query.SearchOptions{
		Limit:     10,
		MaxTokens: 50,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(sr.Results) != 2 {
		t.Errorf("expected 2 results within budget, got %d", len(sr.Results))
	}
	if !sr.Truncated {
		t.Error("expected Truncated=true")
	}
}

func TestQuerier_SearchWithOptions_MaxTokens_Zero(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{ID: "r1", FilePath: "a.go", Language: "go", Content: "func a(){}", ContentHash: "h1", NodeKind: "fn", Name: "a", Embedding: []float32{1, 0, 0}},
		{ID: "r2", FilePath: "b.go", Language: "go", Content: "func b(){}", ContentHash: "h2", NodeKind: "fn", Name: "b", Embedding: []float32{0.9, 0.1, 0}},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	sr, err := q.SearchWithOptions(ctx, "func", query.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(sr.Results) != 2 {
		t.Errorf("expected 2 results, got %d", len(sr.Results))
	}
	if sr.Truncated {
		t.Error("expected Truncated=false when no budget")
	}
}

func TestQuerier_SearchWithOptions_MaxTokens_AlwaysOneResult(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{ID: "r1", FilePath: "a.go", Language: "go", Content: strings.Repeat("x", 1000), ContentHash: "h1", NodeKind: "fn", Name: "a", Embedding: []float32{1, 0, 0}},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	sr, err := q.SearchWithOptions(ctx, "test", query.SearchOptions{
		Limit:     10,
		MaxTokens: 1,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(sr.Results) != 1 {
		t.Errorf("expected 1 result (always include first), got %d", len(sr.Results))
	}
	if sr.Truncated {
		t.Error("expected Truncated=false when only result included")
	}
}

type countingEmbedder struct {
	vec   []float32
	calls atomic.Int32
}

func (c *countingEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	c.calls.Add(1)
	return c.vec, nil
}

func (c *countingEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	c.calls.Add(1)
	result := make([][]float32, len(texts))
	for i := range texts {
		result[i] = c.vec
	}
	return result, nil
}

func TestQuerier_CacheHit(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "main.go", Language: "go",
			Content: "func main() {}", ContentHash: "h1", NodeKind: "function",
			Name: "main", StartLine: 1, EndLine: 3,
			Embedding: []float32{1, 0, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &countingEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	if _, err := q.Search(ctx, "main function", 10); err != nil {
		t.Fatalf("first search: %v", err)
	}
	if emb.calls.Load() != 1 {
		t.Fatalf("expected 1 embedder call after first search, got %d", emb.calls.Load())
	}

	if _, err := q.Search(ctx, "main function", 10); err != nil {
		t.Fatalf("second search: %v", err)
	}
	if emb.calls.Load() != 1 {
		t.Errorf("expected 1 embedder call after cached search, got %d", emb.calls.Load())
	}

	if _, err := q.Search(ctx, "different query", 10); err != nil {
		t.Fatalf("third search: %v", err)
	}
	if emb.calls.Load() != 2 {
		t.Errorf("expected 2 embedder calls after new query, got %d", emb.calls.Load())
	}
}

func TestQuerier_SearchWithOptions_PackageFilter(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "store/sqlite.go", Language: "go",
			Content: "func Search() {}", ContentHash: "h1", NodeKind: "function",
			Name: "Search", StartLine: 1, EndLine: 5,
			Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "indexer/indexer.go", Language: "go",
			Content: "func Index() {}", ContentHash: "h2", NodeKind: "function",
			Name: "Index", StartLine: 1, EndLine: 5,
			Embedding: []float32{0.9, 0.1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	sr, err := q.SearchWithOptions(ctx, "function", query.SearchOptions{
		Limit:   10,
		Package: "store",
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, r := range sr.Results {
		if r.FilePath == "indexer/indexer.go" {
			t.Error("package filter should exclude indexer/indexer.go")
		}
	}
	if len(sr.Results) != 1 || sr.Results[0].FilePath != "store/sqlite.go" {
		t.Errorf("expected exactly [store/sqlite.go], got %v", sr.Results)
	}
}

func TestQuerier_Map_AllOptionsZeroValue(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/main.go", Language: "go",
			Content: "func Main() {}", ContentHash: "h1",
			NodeKind: "function_declaration", Name: "Main", Parent: "",
			StartLine: 1, EndLine: 10, Embedding: []float32{1, 0, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	// Zero-value: no visibility markers, no summary. But line ranges always shown.
	if !strings.Contains(result, ":1-10") {
		t.Error("line ranges should always appear")
	}
	if strings.Contains(result, "+ ") || strings.Contains(result, "- ") {
		t.Error("zero-value MapOptions should not show visibility markers")
	}
	if !strings.Contains(result, "func Main") {
		t.Error("expected 'func Main' in output")
	}
}

func TestQuerier_Map_CodeOnlyFilter(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/main.go", Language: "go",
			Content: "func Main() {}", ContentHash: "h1",
			NodeKind: "function_declaration", Name: "Main", Parent: "",
			StartLine: 1, EndLine: 10, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/CHANGELOG.md", Language: "markdown",
			Content: "# v1.0", ContentHash: "h2",
			NodeKind: "atx_heading", Name: "v1.0", Parent: "",
			StartLine: 1, EndLine: 1, Embedding: []float32{0, 1, 0},
		},
		{
			ID: "r3", FilePath: "/repo/config.yaml", Language: "yaml",
			Content: "key: value", ContentHash: "h3",
			NodeKind: "block_mapping_pair", Name: "key", Parent: "",
			StartLine: 1, EndLine: 1, Embedding: []float32{0, 0, 1},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	// CodeOnly=true: markdown and yaml excluded.
	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000, CodeOnly: true, NonCodeLanguages: []string{"markdown", "yaml", "json"}})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result, "func Main") {
		t.Error("expected Go symbol in code-only output")
	}
	if strings.Contains(result, "CHANGELOG") {
		t.Error("expected markdown file excluded in code-only output")
	}
	if strings.Contains(result, "config.yaml") {
		t.Error("expected yaml file excluded in code-only output")
	}

	// CodeOnly=false: all languages included (no cache invalidation needed —
	// cache stores unfiltered data, filtering is per-call).
	result2, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000, CodeOnly: false})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result2, "CHANGELOG") {
		t.Error("expected markdown file in non-code-only output")
	}
	if !strings.Contains(result2, "config.yaml") {
		t.Error("expected yaml file in non-code-only output")
	}
}

func TestQuerier_Map_LineRanges(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/main.go", Language: "go",
			Content: "func Main() {}", ContentHash: "h1",
			NodeKind: "function_declaration", Name: "Main", Parent: "",
			StartLine: 10, EndLine: 25, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/main.go", Language: "go",
			Content: "var X = 1", ContentHash: "h2",
			NodeKind: "var_declaration", Name: "X", Parent: "",
			StartLine: 42, EndLine: 42, Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result, "Main:10-25") {
		t.Errorf("expected 'Main:10-25' in output, got:\n%s", result)
	}
	// Single-line symbol should render :42 not :42-42.
	if !strings.Contains(result, "X:42") {
		t.Errorf("expected 'X:42' in output, got:\n%s", result)
	}
	if strings.Contains(result, "X:42-42") {
		t.Errorf("single-line symbol should not render :42-42, got:\n%s", result)
	}
}

func TestQuerier_Map_FileSummary(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// 5 symbols in one file to trigger summary (threshold is 5).
	var records []store.Record
	for i := range 4 {
		records = append(records, store.Record{
			ID: fmt.Sprintf("rf%d", i), FilePath: "/repo/big.go", Language: "go",
			Content: fmt.Sprintf("func F%d() {}", i), ContentHash: fmt.Sprintf("hf%d", i),
			NodeKind: "function_declaration", Name: fmt.Sprintf("F%d", i), Parent: "",
			StartLine: i*10 + 1, EndLine: i*10 + 8, Embedding: []float32{1, 0, 0},
		})
	}
	records = append(records, store.Record{
		ID: "rt", FilePath: "/repo/big.go", Language: "go",
		Content: "type BigType struct{}", ContentHash: "ht",
		NodeKind: "type_spec", Name: "BigType", Parent: "",
		StartLine: 50, EndLine: 60, Embedding: []float32{0, 1, 0},
	})
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000, ShowSummary: true})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	// Expect "4 func" and "1 type" in the file header.
	if !strings.Contains(result, "4 func") {
		t.Errorf("expected '4 func' in summary, got:\n%s", result)
	}
	if !strings.Contains(result, "1 type") {
		t.Errorf("expected '1 type' in summary, got:\n%s", result)
	}
}

func TestQuerier_Map_Visibility(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/server.go", Language: "go",
			Content: "func NewServer() {}", ContentHash: "h1",
			NodeKind: "function_declaration", Name: "NewServer", Parent: "",
			StartLine: 10, EndLine: 25, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/server.go", Language: "go",
			Content: "func helper() {}", ContentHash: "h2",
			NodeKind: "function_declaration", Name: "helper", Parent: "",
			StartLine: 30, EndLine: 40, Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000, ShowVisibility: true})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result, "+ func NewServer") {
		t.Errorf("expected '+ func NewServer' for exported Go symbol, got:\n%s", result)
	}
	if !strings.Contains(result, "- func helper") {
		t.Errorf("expected '- func helper' for unexported Go symbol, got:\n%s", result)
	}
}

func TestQuerier_Map_VisibilityPython(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/app.py", Language: "python",
			Content: "def public_func(): pass", ContentHash: "h1",
			NodeKind: "function_definition", Name: "public_func", Parent: "",
			StartLine: 1, EndLine: 2, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/app.py", Language: "python",
			Content: "def _private(): pass", ContentHash: "h2",
			NodeKind: "function_definition", Name: "_private", Parent: "",
			StartLine: 5, EndLine: 6, Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000, ShowVisibility: true})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result, "+ func public_func") {
		t.Errorf("expected '+ func public_func', got:\n%s", result)
	}
	if !strings.Contains(result, "- func _private") {
		t.Errorf("expected '- func _private', got:\n%s", result)
	}
}

func TestQuerier_Map_VisibilityJSExport(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/index.ts", Language: "typescript",
			Content: "export function greet() {}", ContentHash: "h1",
			NodeKind: "export_statement", Name: "greet", Parent: "",
			StartLine: 1, EndLine: 3, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/index.ts", Language: "typescript",
			Content: "function internal() {}", ContentHash: "h2",
			NodeKind: "function_declaration", Name: "internal", Parent: "",
			StartLine: 5, EndLine: 7, Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000, ShowVisibility: true})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result, "+ export greet") {
		t.Errorf("expected '+ export greet' for exported TS symbol, got:\n%s", result)
	}
	if !strings.Contains(result, "- func internal") {
		t.Errorf("expected '- func internal' for non-exported TS symbol, got:\n%s", result)
	}
}

func TestQuerier_Map_NestingHierarchy(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/server.go", Language: "go",
			Content: "type Server struct{}", ContentHash: "h1",
			NodeKind: "type_spec", Name: "Server", Parent: "",
			StartLine: 5, EndLine: 10, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/server.go", Language: "go",
			Content: "func (s *Server) Handle() {}", ContentHash: "h2",
			NodeKind: "method_declaration", Name: "Handle", Parent: "Server",
			StartLine: 15, EndLine: 25, Embedding: []float32{0, 1, 0},
		},
		{
			ID: "r3", FilePath: "/repo/server.go", Language: "go",
			Content: "func (s *Server) Serve() {}", ContentHash: "h3",
			NodeKind: "method_declaration", Name: "Serve", Parent: "Server",
			StartLine: 30, EndLine: 40, Embedding: []float32{0, 0, 1},
		},
		{
			ID: "r4", FilePath: "/repo/server.go", Language: "go",
			Content: "func NewServer() {}", ContentHash: "h4",
			NodeKind: "function_declaration", Name: "NewServer", Parent: "",
			StartLine: 45, EndLine: 55, Embedding: []float32{0.5, 0.5, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}

	// Handle and Serve should appear after Server (grouped under it).
	serverIdx := strings.Index(result, "type Server")
	handleIdx := strings.Index(result, "func Handle")
	serveIdx := strings.Index(result, "func Serve")
	newServerIdx := strings.Index(result, "func NewServer")

	if serverIdx == -1 || handleIdx == -1 || serveIdx == -1 || newServerIdx == -1 {
		t.Fatalf("missing symbols in output:\n%s", result)
	}
	if handleIdx < serverIdx {
		t.Error("Handle should appear after Server (nested)")
	}
	if serveIdx < serverIdx {
		t.Error("Serve should appear after Server (nested)")
	}
	// Handle and Serve should be indented 4 spaces (children).
	if !strings.Contains(result, "    func Handle") {
		t.Errorf("expected 4-space indent for Handle, got:\n%s", result)
	}
	if !strings.Contains(result, "    func Serve") {
		t.Errorf("expected 4-space indent for Serve, got:\n%s", result)
	}
	// NewServer is top-level, should be 2-space indent.
	if !strings.Contains(result, "  func NewServer") {
		t.Errorf("expected 2-space indent for NewServer, got:\n%s", result)
	}
}

func TestQuerier_Map_OrphanChildren(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/orphan.go", Language: "go",
			Content: "func Method() {}", ContentHash: "h1",
			NodeKind: "method_declaration", Name: "Method", Parent: "Missing",
			StartLine: 10, EndLine: 20, Embedding: []float32{1, 0, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	result, err := q.Map(ctx, query.MapOptions{MaxTokens: 2000})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	// Orphan should render at top level (2-space indent), not crash.
	if !strings.Contains(result, "  func Method") {
		t.Errorf("expected orphan child at 2-space indent, got:\n%s", result)
	}
}

func TestQuerier_Map_CodeOnlyCustomLanguages(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/main.go", Language: "go",
			Content: "func Main() {}", ContentHash: "h1",
			NodeKind: "function_declaration", Name: "Main", Parent: "",
			StartLine: 1, EndLine: 10, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/style.css", Language: "css",
			Content: ".body { color: red; }", ContentHash: "h2",
			NodeKind: "rule_set", Name: "body", Parent: "",
			StartLine: 1, EndLine: 1, Embedding: []float32{0, 1, 0},
		},
		{
			ID: "r3", FilePath: "/repo/index.html", Language: "html",
			Content: "<div>hello</div>", ContentHash: "h3",
			NodeKind: "element", Name: "div", Parent: "",
			StartLine: 1, EndLine: 1, Embedding: []float32{0, 0, 1},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	// Custom list includes css and html but not the defaults.
	result, err := q.Map(ctx, query.MapOptions{
		MaxTokens:        2000,
		CodeOnly:         true,
		NonCodeLanguages: []string{"css", "html"},
	})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result, "func Main") {
		t.Error("expected Go symbol in output")
	}
	if strings.Contains(result, "style.css") {
		t.Error("expected css file excluded with custom non-code languages")
	}
	if strings.Contains(result, "index.html") {
		t.Error("expected html file excluded with custom non-code languages")
	}
}

func TestQuerier_Map_CodeOnlyEmptyList(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	records := []store.Record{
		{
			ID: "r1", FilePath: "/repo/main.go", Language: "go",
			Content: "func Main() {}", ContentHash: "h1",
			NodeKind: "function_declaration", Name: "Main", Parent: "",
			StartLine: 1, EndLine: 10, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "/repo/README.md", Language: "markdown",
			Content: "# Title", ContentHash: "h2",
			NodeKind: "atx_heading", Name: "Title", Parent: "",
			StartLine: 1, EndLine: 1, Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)

	// Empty NonCodeLanguages with CodeOnly=true filters nothing.
	result, err := q.Map(ctx, query.MapOptions{
		MaxTokens:        2000,
		CodeOnly:         true,
		NonCodeLanguages: []string{},
	})
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if !strings.Contains(result, "func Main") {
		t.Error("expected Go symbol in output")
	}
	if !strings.Contains(result, "README.md") {
		t.Error("empty NonCodeLanguages should not filter any files")
	}
}
