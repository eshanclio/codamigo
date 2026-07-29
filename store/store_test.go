package store_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ieshan/codamigo/store"
)

func TestRecordID(t *testing.T) {
	id1 := store.RecordID("src/main.go", "func main() {}")
	id2 := store.RecordID("src/main.go", "func main() {}")
	id3 := store.RecordID("src/other.go", "func main() {}")
	id4 := store.RecordID("src/main.go", "func other() {}")

	if id1 != id2 {
		t.Errorf("same input should produce same ID: got %q and %q", id1, id2)
	}
	if id1 == id3 {
		t.Error("different file paths should produce different IDs")
	}
	if id1 == id4 {
		t.Error("different content should produce different IDs")
	}
	if len(id1) != 64 {
		t.Errorf("expected 64-char hex SHA-256, got %d chars", len(id1))
	}
}

func TestContentHash(t *testing.T) {
	h1 := store.ContentHash([]byte("func main() {}"))
	h2 := store.ContentHash([]byte("func main() {}"))
	h3 := store.ContentHash([]byte("func other() {}"))

	if h1 != h2 {
		t.Errorf("same content should produce same hash: got %q and %q", h1, h2)
	}
	if h1 == h3 {
		t.Error("different content should produce different hashes")
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex SHA-256, got %d chars", len(h1))
	}
}

func testDB(t *testing.T) string {
	t.Helper()
	return ":memory:"
}

func TestNewSQLiteStore(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()
}

func TestNewSQLiteStore_MetadataPersisted(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"

	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}

	ctx := t.Context()
	v, err := s.Meta(ctx, "embedding_model")
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if v != "test-model" {
		t.Errorf("embedding_model = %q, want %q", v, "test-model")
	}
	s.Close()

	s2, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer s2.Close()
}

func TestNewSQLiteStore_ModelMismatch(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"

	s, err := store.NewSQLiteStore(dbPath, "model-a", 3)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s.Close()

	_, err = store.NewSQLiteStore(dbPath, "model-b", 3)
	if err == nil {
		t.Fatal("expected error on model mismatch, got nil")
	}
}

func TestNewSQLiteStore_DimMismatch(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"

	s, err := store.NewSQLiteStore(dbPath, "model-a", 3)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	s.Close()

	_, err = store.NewSQLiteStore(dbPath, "model-a", 768)
	if err == nil {
		t.Fatal("expected error on dimension mismatch, got nil")
	}
}

// An index written by an older codamigo must be rejected outright rather than
// read with a schema the code no longer expects. There is no in-place migration:
// the fix is to delete the database and re-index. Like the two tests above this
// needs a real file, because the rejection only happens when an existing
// database is reopened.
func TestNewSQLiteStore_SchemaVersionMismatch(t *testing.T) {
	dbPath := t.TempDir() + "/test.db"

	s, err := store.NewSQLiteStore(dbPath, "model-a", 3)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err = s.SetMeta(t.Context(), "schema_version", "2"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	s.Close()

	_, err = store.NewSQLiteStore(dbPath, "model-a", 3)
	if err == nil {
		t.Fatal("expected error on schema version mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "re-index required") {
		t.Errorf("error should tell the user to re-index, got: %v", err)
	}
}

func TestNewSQLiteStore_InvalidPath(t *testing.T) {
	_, err := store.NewSQLiteStore("/nonexistent/dir/test.db", "model", 3)
	if err == nil {
		t.Fatal("expected error for invalid path, got nil")
	}
}

func TestNewSQLiteStore_InvalidDim(t *testing.T) {
	tests := []struct {
		name string
		dim  int
	}{
		{name: "zero", dim: 0},
		{name: "negative", dim: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.NewSQLiteStore(testDB(t), "model", tt.dim)
			if err == nil {
				t.Fatalf("expected error for dim %d, got nil", tt.dim)
			}
		})
	}
}

func TestCheckpoint(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	r := makeRecord("main.go", "main", "func main() {}", []float32{0.1, 0.2, 0.3})
	if err = s.Upsert(ctx, []store.Record{r}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err = s.Checkpoint(ctx); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
}

func makeRecord(filePath, name, content string, embedding []float32) store.Record {
	return store.Record{
		ID:          store.RecordID(filePath, content),
		FilePath:    filePath,
		Language:    "go",
		Content:     content,
		ContentHash: store.ContentHash([]byte(content)),
		NodeKind:    "function_declaration",
		Name:        name,
		Parent:      "",
		StartLine:   1,
		EndLine:     5,
		Embedding:   embedding,
	}
}

func TestUpsert_and_Delete(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	r := makeRecord("main.go", "main", "func main() {}", []float32{0.1, 0.2, 0.3})

	if err = s.Upsert(ctx, []store.Record{r}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Upsert again should not error (idempotent).
	if err = s.Upsert(ctx, []store.Record{r}); err != nil {
		t.Fatalf("Upsert idempotent: %v", err)
	}

	if err = s.Delete(ctx, []string{r.ID}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestUpsert_BatchMultipleRecords(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		makeRecord("a.go", "funcA", "func A() {}", []float32{0.1, 0.2, 0.3}),
		makeRecord("b.go", "funcB", "func B() {}", []float32{0.4, 0.5, 0.6}),
		makeRecord("c.go", "funcC", "func C() {}", []float32{0.7, 0.8, 0.9}),
	}

	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("Upsert batch: %v", err)
	}
}

func TestDeleteByFile(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		makeRecord("main.go", "funcA", "func A() {}", []float32{0.1, 0.2, 0.3}),
		makeRecord("main.go", "funcB", "func B() {}", []float32{0.4, 0.5, 0.6}),
		makeRecord("other.go", "funcC", "func C() {}", []float32{0.7, 0.8, 0.9}),
	}

	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err = s.DeleteByFile(ctx, "main.go"); err != nil {
		t.Fatalf("DeleteByFile: %v", err)
	}

	// "other.go" record should still exist — verify via ChunkHashesByFile.
	hashes, err := s.ChunkHashesByFile(ctx, "other.go")
	if err != nil {
		t.Fatalf("ChunkHashesByFile: %v", err)
	}
	if len(hashes) != 1 {
		t.Errorf("expected 1 chunk for other.go, got %d", len(hashes))
	}

	// "main.go" should have no chunks.
	hashes, err = s.ChunkHashesByFile(ctx, "main.go")
	if err != nil {
		t.Fatalf("ChunkHashesByFile: %v", err)
	}
	if len(hashes) != 0 {
		t.Errorf("expected 0 chunks for main.go after delete, got %d", len(hashes))
	}
}

func TestUpsert_WrongEmbeddingDimension(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	r := makeRecord("main.go", "main", "func main() {}", []float32{0.1, 0.2, 0.3, 0.4}) // 4 floats, store expects 3
	if err := s.Upsert(ctx, []store.Record{r}); err == nil {
		t.Fatal("expected error for wrong embedding dimension, got nil")
	}
}

func TestUpsert_EmptySlice(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	if err = s.Upsert(ctx, nil); err != nil {
		t.Fatalf("Upsert nil: %v", err)
	}
	if err = s.Upsert(ctx, []store.Record{}); err != nil {
		t.Fatalf("Upsert empty: %v", err)
	}
}

func TestFileHashes(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	// No files yet — query should return empty map.
	hashes, err := s.FileHashes(ctx, []string{"main.go"})
	if err != nil {
		t.Fatalf("FileHashes not-found: %v", err)
	}
	if len(hashes) != 0 {
		t.Errorf("expected empty map for missing file, got %v", hashes)
	}

	// Set a file hash via ReplaceByFiles (empty records = hash-only entry).
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: "main.go", FileHash: "abc123"}}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	hashes, err = s.FileHashes(ctx, []string{"main.go"})
	if err != nil {
		t.Fatalf("FileHashes after set: %v", err)
	}
	if hashes["main.go"] != "abc123" {
		t.Errorf("FileHashes = %q, want %q", hashes["main.go"], "abc123")
	}

	// Update should work.
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: "main.go", FileHash: "def456"}}); err != nil {
		t.Fatalf("ReplaceByFiles update: %v", err)
	}
	hashes, err = s.FileHashes(ctx, []string{"main.go"})
	if err != nil {
		t.Fatalf("FileHashes after update: %v", err)
	}
	if hashes["main.go"] != "def456" {
		t.Errorf("FileHashes after update = %q, want %q", hashes["main.go"], "def456")
	}
}

func TestListFiles(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()

	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles empty: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}

	if err = s.ReplaceByFiles(ctx, []store.FileRecords{
		{FilePath: "a.go", FileHash: "hash1"},
		{FilePath: "b.go", FileHash: "hash2"},
	}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	files, err = s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d", len(files))
	}
}

func TestSearch_VectorOnly(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		makeRecord("a.go", "funcA", "func handleAuth() { validateToken() }", []float32{0.9, 0.1, 0.0}),
		makeRecord("b.go", "funcB", "func handlePayment() { chargeCard() }", []float32{0.0, 0.9, 0.1}),
		makeRecord("c.go", "funcC", "func handleLogging() { writeLog() }", []float32{0.1, 0.0, 0.9}),
	}

	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{0.9, 0.1, 0.0},
		Text:      "",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].Name != "funcA" {
		t.Errorf("expected funcA as top result, got %q", results[0].Name)
	}
}

func TestSearch_BM25Only(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		makeRecord("a.go", "parseConfig", "func parseConfig() { readYAML() }", []float32{0.5, 0.5, 0.0}),
		makeRecord("b.go", "parseArgs", "func parseArgs() { flag.Parse() }", []float32{0.5, 0.0, 0.5}),
		makeRecord("c.go", "handleAuth", "func handleAuth() { validateJWT() }", []float32{0.0, 0.5, 0.5}),
	}

	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: nil, // nil = skip vector search, pure BM25
		Text:      "parseConfig",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected at least 1 result")
	}

	if results[0].Name != "parseConfig" {
		t.Errorf("expected parseConfig as top BM25 result, got %q", results[0].Name)
	}
}

func TestSearch_HybridMerge(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		makeRecord("a.go", "parseConfig", "func parseConfig() { readYAML() }", []float32{0.9, 0.1, 0.0}),
		makeRecord("b.go", "loadConfig", "func loadConfig() { openFile() }", []float32{0.85, 0.15, 0.0}),
		makeRecord("c.go", "unrelated", "func unrelated() { doStuff() }", []float32{0.0, 0.0, 1.0}),
	}

	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Query with both vector similarity to parseConfig AND text match.
	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{0.88, 0.12, 0.0},
		Text:      "parseConfig",
		Limit:     3,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected results")
	}

	// parseConfig should be top — it matches both vector and keyword.
	if results[0].Name != "parseConfig" {
		t.Errorf("expected parseConfig as top hybrid result, got %q", results[0].Name)
	}
}

func TestSearch_LanguageFilter(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	goRecord := makeRecord("main.go", "main", "func main() {}", []float32{0.9, 0.1, 0.0})
	goRecord.Language = "go"

	pyRecord := makeRecord("main.py", "main", "def main(): pass", []float32{0.85, 0.1, 0.05})
	pyRecord.Language = "python"

	if err = s.Upsert(ctx, []store.Record{goRecord, pyRecord}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{0.9, 0.1, 0.0},
		Text:      "main",
		Limit:     10,
		Languages: []string{"python"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, r := range results {
		if r.Language != "python" {
			t.Errorf("expected only python results, got language %q", r.Language)
		}
	}
}

func TestSearch_EmptyStore(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{0.1, 0.2, 0.3},
		Text:      "anything",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Search empty store: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results from empty store, got %d", len(results))
	}
}

func TestEmbeddingsByContentHash(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	r1 := makeRecord("a.go", "funcA", "func A() {}", []float32{0.1, 0.2, 0.3})
	r2 := makeRecord("b.go", "funcB", "func B() {}", []float32{0.4, 0.5, 0.6})

	if err = s.Upsert(ctx, []store.Record{r1, r2}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	embeddings, err := s.EmbeddingsByContentHash(ctx, []string{r1.ContentHash, r2.ContentHash, "nonexistent"})
	if err != nil {
		t.Fatalf("EmbeddingsByContentHash: %v", err)
	}

	if len(embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(embeddings))
	}

	emb1 := embeddings[r1.ContentHash]
	if len(emb1) != 3 || emb1[0] != 0.1 || emb1[1] != 0.2 || emb1[2] != 0.3 {
		t.Errorf("embedding for r1: got %v, want [0.1, 0.2, 0.3]", emb1)
	}

	emb2 := embeddings[r2.ContentHash]
	if len(emb2) != 3 || emb2[0] != 0.4 || emb2[1] != 0.5 || emb2[2] != 0.6 {
		t.Errorf("embedding for r2: got %v, want [0.4, 0.5, 0.6]", emb2)
	}
}

func TestSearch_PathFilter(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	records := []store.Record{
		{
			ID: "r1", FilePath: "src/main.go", Language: "go",
			Content: "func main", ContentHash: "h1", NodeKind: "function",
			Name: "main", StartLine: 1, EndLine: 3,
			Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "src/utils/helper.go", Language: "go",
			Content: "func helper", ContentHash: "h2", NodeKind: "function",
			Name: "helper", StartLine: 1, EndLine: 5,
			Embedding: []float32{0.9, 0.1, 0},
		},
		{
			ID: "r3", FilePath: "test/main_test.go", Language: "go",
			Content: "func TestMain", ContentHash: "h3", NodeKind: "function",
			Name: "TestMain", StartLine: 1, EndLine: 10,
			Embedding: []float32{0.8, 0.2, 0},
		},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Limit:     10,
		Paths:     []string{"src/**"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	for _, r := range results {
		if r.FilePath == "test/main_test.go" {
			t.Errorf("path filter should exclude test/main_test.go, but it was returned")
		}
	}

	// Also assert the src/ files ARE returned
	found := make(map[string]bool)
	for _, r := range results {
		found[r.FilePath] = true
	}
	if !found["src/main.go"] {
		t.Error("expected src/main.go in results")
	}
	if !found["src/utils/helper.go"] {
		t.Error("expected src/utils/helper.go in results")
	}
	if len(results) != 2 {
		t.Errorf("expected exactly 2 results, got %d", len(results))
	}
}

func TestSearch_PathFilter_Empty(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	records := []store.Record{
		{
			ID: "r1", FilePath: "src/main.go", Language: "go",
			Content: "func main", ContentHash: "h1", NodeKind: "function",
			Name: "main", StartLine: 1, EndLine: 3,
			Embedding: []float32{1, 0, 0},
		},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Empty Paths should return all results (no filtering).
	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Limit:     10,
		Paths:     nil,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result with empty Paths, got %d", len(results))
	}
}

func TestNewSQLiteStore_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nested", "subdir", "store.db")

	s, err := store.NewSQLiteStore(dbPath, "text-embedding-3-small", 4)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	if _, err = os.Stat(filepath.Join(dir, "nested", "subdir")); errors.Is(err, fs.ErrNotExist) {
		t.Error("NewSQLiteStore did not create nested directory")
	}
}

func TestSQLiteStore_SearchOffset(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(testDB(t), "m", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	records := []store.Record{
		{ID: "a", FilePath: "a.go", Language: "go", Content: "alpha", ContentHash: "h1", NodeKind: "fn", Embedding: []float32{1, 0, 0}},
		{ID: "b", FilePath: "b.go", Language: "go", Content: "beta", ContentHash: "h2", NodeKind: "fn", Embedding: []float32{0.9, 0.1, 0}},
		{ID: "c", FilePath: "c.go", Language: "go", Content: "gamma", ContentHash: "h3", NodeKind: "fn", Embedding: []float32{0.8, 0.2, 0}},
		{ID: "d", FilePath: "d.go", Language: "go", Content: "delta", ContentHash: "h4", NodeKind: "fn", Embedding: []float32{0.7, 0.3, 0}},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	all, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "",
		Limit:     4,
		Offset:    0,
	})
	if err != nil {
		t.Fatalf("search all: %v", err)
	}

	paged, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "",
		Limit:     2,
		Offset:    2,
	})
	if err != nil {
		t.Fatalf("search paged: %v", err)
	}

	if len(paged) == 0 {
		t.Fatal("expected paged results, got none")
	}
	// Paged results must not overlap with the first 2 all-results.
	topTwo := map[string]bool{all[0].ID: true, all[1].ID: true}
	for _, r := range paged {
		if topTwo[r.ID] {
			t.Errorf("result %q appeared in both offset=0 and offset=2 pages", r.ID)
		}
	}
}

func TestSQLiteStore_Stats(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(testDB(t), "m", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	// Empty store.
	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("stats on empty store: %v", err)
	}
	if stats.ChunkCount != 0 || stats.FileCount != 0 {
		t.Errorf("empty store: want 0/0, got %d/%d", stats.ChunkCount, stats.FileCount)
	}

	if err = s.ReplaceByFiles(ctx, []store.FileRecords{
		{FilePath: "main.go", FileHash: "fh1", Records: []store.Record{
			{ID: "g1", FilePath: "main.go", Language: "go", Content: "func a(){}", ContentHash: "h1", NodeKind: "fn", Embedding: []float32{1, 0, 0}},
			{ID: "g2", FilePath: "main.go", Language: "go", Content: "func b(){}", ContentHash: "h2", NodeKind: "fn", Embedding: []float32{0, 1, 0}},
		}},
		{FilePath: "script.py", FileHash: "fh2", Records: []store.Record{
			{ID: "p1", FilePath: "script.py", Language: "python", Content: "def c():", ContentHash: "h3", NodeKind: "fn", Embedding: []float32{0, 0, 1}},
		}},
	}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	stats, err = s.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.ChunkCount != 3 {
		t.Errorf("ChunkCount: want 3, got %d", stats.ChunkCount)
	}
	if stats.FileCount != 2 {
		t.Errorf("FileCount: want 2, got %d", stats.FileCount)
	}
	if stats.Languages["go"] != 2 {
		t.Errorf("go chunks: want 2, got %d", stats.Languages["go"])
	}
	if stats.Languages["python"] != 1 {
		t.Errorf("python chunks: want 1, got %d", stats.Languages["python"])
	}
}

func TestIntegration_FullWorkflow(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()

	// 1. Index two files.
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{
		{FilePath: "auth.go", FileHash: "hash-auth-v1", Records: []store.Record{
			makeRecord("auth.go", "validateToken", "func validateToken(jwt string) error { return nil }", []float32{0.9, 0.1, 0.0}),
			makeRecord("auth.go", "refreshToken", "func refreshToken(old string) (string, error) { return old, nil }", []float32{0.8, 0.2, 0.0}),
		}},
		{FilePath: "db.go", FileHash: "hash-db-v1", Records: []store.Record{
			makeRecord("db.go", "openDB", "func openDB(dsn string) (*sql.DB, error) { return nil, nil }", []float32{0.0, 0.9, 0.1}),
		}},
	}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	// 2. Verify file tracking.
	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Errorf("expected 2 files, got %d", len(files))
	}

	hashes, err := s.FileHashes(ctx, []string{"auth.go"})
	if err != nil {
		t.Fatalf("FileHashes: %v", err)
	}
	if hashes["auth.go"] != "hash-auth-v1" {
		t.Errorf("FileHashes = %q, want %q", hashes["auth.go"], "hash-auth-v1")
	}

	// 3. Verify chunk hash tracking.
	hashes, err = s.ChunkHashesByFile(ctx, "auth.go")
	if err != nil {
		t.Fatalf("ChunkHashesByFile: %v", err)
	}
	if len(hashes) != 2 {
		t.Errorf("expected 2 chunk hashes for auth.go, got %d", len(hashes))
	}

	// 4. Verify embedding reuse.
	contentHashes := make([]string, 0, len(hashes))
	for _, ch := range hashes {
		contentHashes = append(contentHashes, ch)
	}
	embeddings, err := s.EmbeddingsByContentHash(ctx, contentHashes)
	if err != nil {
		t.Fatalf("EmbeddingsByContentHash: %v", err)
	}
	if len(embeddings) != 2 {
		t.Errorf("expected 2 reusable embeddings, got %d", len(embeddings))
	}

	// 5. Search for auth-related code.
	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{0.85, 0.15, 0.0},
		Text:      "validateToken",
		Limit:     2,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results")
	}
	if results[0].Name != "validateToken" {
		t.Errorf("top result = %q, want validateToken", results[0].Name)
	}

	// 6. Simulate file deletion: delete auth.go.
	if err = s.DeleteByFile(ctx, "auth.go"); err != nil {
		t.Fatalf("DeleteByFile: %v", err)
	}

	hashes, _ = s.ChunkHashesByFile(ctx, "auth.go")
	if len(hashes) != 0 {
		t.Errorf("expected 0 chunks after delete, got %d", len(hashes))
	}

	// 7. db.go should still be searchable.
	results, err = s.Search(ctx, store.SearchQuery{
		Embedding: []float32{0.0, 0.9, 0.1},
		Text:      "openDB",
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("Search after delete: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result after delete, got %d", len(results))
	}
	if results[0].Name != "openDB" {
		t.Errorf("expected openDB, got %q", results[0].Name)
	}

	// 8. Verify metadata.
	model, _ := s.Meta(ctx, "embedding_model")
	if model != "test-model" {
		t.Errorf("Meta embedding_model = %q, want %q", model, "test-model")
	}
}

func TestListSymbols(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		{
			ID: "r1", FilePath: "store/store.go", Language: "go",
			Content: "type Store interface{}", ContentHash: "h1",
			NodeKind: "type_declaration", Name: "Store", Parent: "",
			StartLine: 10, EndLine: 20, Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "store/store.go", Language: "go",
			Content: "func Search() {}", ContentHash: "h2",
			NodeKind: "function_declaration", Name: "Search", Parent: "Store",
			StartLine: 30, EndLine: 40, Embedding: []float32{0, 1, 0},
		},
		{
			ID: "r3", FilePath: "indexer/indexer.go", Language: "go",
			Content: "func Index() {}", ContentHash: "h3",
			NodeKind: "function_declaration", Name: "Index", Parent: "",
			StartLine: 5, EndLine: 15, Embedding: []float32{0, 0, 1},
		},
		{
			ID: "r4", FilePath: "store/store.go", Language: "go",
			Content: "// comment block", ContentHash: "h4",
			NodeKind: "comment", Name: "", Parent: "",
			StartLine: 1, EndLine: 2, Embedding: []float32{0.1, 0.1, 0.1},
		},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	symbols, err := s.ListSymbols(ctx)
	if err != nil {
		t.Fatalf("ListSymbols: %v", err)
	}

	// Should exclude unnamed chunk (r4).
	if len(symbols) != 3 {
		t.Fatalf("expected 3 symbols, got %d", len(symbols))
	}

	// Should be ordered by file_path then start_line.
	if symbols[0].FilePath != "indexer/indexer.go" {
		t.Errorf("expected first symbol from indexer/indexer.go, got %s", symbols[0].FilePath)
	}
	if symbols[1].Name != "Store" || symbols[2].Name != "Search" {
		t.Errorf("expected Store then Search in store/store.go, got %s then %s", symbols[1].Name, symbols[2].Name)
	}

	// Verify fields.
	if symbols[2].Parent != "Store" {
		t.Errorf("expected Parent='Store' for Search, got %q", symbols[2].Parent)
	}
	if symbols[2].NodeKind != "function_declaration" {
		t.Errorf("expected NodeKind='function_declaration', got %q", symbols[2].NodeKind)
	}

	// Verify EndLine and Language fields are populated.
	if symbols[0].EndLine != 15 {
		t.Errorf("expected EndLine=15 for first symbol (Index), got %d", symbols[0].EndLine)
	}
	if symbols[0].Language != "go" {
		t.Errorf("expected Language=%q for first symbol (Index), got %q", "go", symbols[0].Language)
	}
	if symbols[1].EndLine != 20 {
		t.Errorf("expected EndLine=20 for second symbol (Store), got %d", symbols[1].EndLine)
	}
	if symbols[2].EndLine != 40 {
		t.Errorf("expected EndLine=40 for third symbol (Search), got %d", symbols[2].EndLine)
	}
}

func TestReplaceByFiles(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(testDB(t), "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	oldRecords := []store.Record{
		{
			ID: "old1", FilePath: "/src/main.go", Language: "go",
			Content: "func old1() {}", ContentHash: "h_old1", NodeKind: "function",
			Name: "old1", StartLine: 1, EndLine: 3,
			Embedding: []float32{1, 0, 0},
		},
		{
			ID: "old2", FilePath: "/src/main.go", Language: "go",
			Content: "func old2() {}", ContentHash: "h_old2", NodeKind: "function",
			Name: "old2", StartLine: 5, EndLine: 7,
			Embedding: []float32{0, 1, 0},
		},
	}
	if err = s.Upsert(ctx, oldRecords); err != nil {
		t.Fatalf("initial upsert: %v", err)
	}
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: "/src/main.go", FileHash: "hash_v1"}}); err != nil {
		t.Fatalf("set file hash: %v", err)
	}

	newRecords := []store.Record{
		{
			ID: "new1", FilePath: "/src/main.go", Language: "go",
			Content: "func new1() {}", ContentHash: "h_new1", NodeKind: "function",
			Name: "new1", StartLine: 1, EndLine: 3,
			Embedding: []float32{0, 0, 1},
		},
	}
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: "/src/main.go", Records: newRecords, FileHash: "hash_v2"}}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	hashes, err := s.FileHashes(ctx, []string{"/src/main.go"})
	if err != nil {
		t.Fatalf("file hash: %v", err)
	}
	if hashes["/src/main.go"] != "hash_v2" {
		t.Errorf("file hash: want hash_v2, got %s", hashes["/src/main.go"])
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{0, 0, 1},
		Text:      "new1",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	foundNew := false
	for _, r := range results {
		if r.ID == "old1" || r.ID == "old2" {
			t.Errorf("old record %q should have been deleted", r.ID)
		}
		if r.ID == "new1" {
			foundNew = true
		}
	}
	if !foundNew {
		t.Error("new record 'new1' not found in search results")
	}
}

func TestReplaceByFiles_RollbackOnBadEmbedding(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(testDB(t), "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	oldRecord := store.Record{
		ID: "old1", FilePath: "/src/main.go", Language: "go",
		Content: "func old1() {}", ContentHash: "h_old1", NodeKind: "function",
		Name: "old1", StartLine: 1, EndLine: 3,
		Embedding: []float32{1, 0, 0},
	}
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: "/src/main.go", FileHash: "hash_v1", Records: []store.Record{oldRecord}}}); err != nil {
		t.Fatalf("set file hash: %v", err)
	}

	badRecord := store.Record{
		ID: "new1", FilePath: "/src/main.go", Language: "go",
		Content: "func new1() {}", ContentHash: "h_new1", NodeKind: "function",
		Name: "new1", StartLine: 1, EndLine: 3,
		Embedding: []float32{1, 2}, // wrong dimension
	}
	err = s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: "/src/main.go", Records: []store.Record{badRecord}, FileHash: "hash_v2"}})
	if err == nil {
		t.Fatal("expected error for wrong embedding dimension, got nil")
	}

	hashes, err := s.FileHashes(ctx, []string{"/src/main.go"})
	if err != nil {
		t.Fatalf("file hash: %v", err)
	}
	if hashes["/src/main.go"] != "hash_v1" {
		t.Errorf("file hash should still be hash_v1 after failed write, got %s", hashes["/src/main.go"])
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "old1",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	found := false
	for _, r := range results {
		if r.ID == "old1" {
			found = true
		}
	}
	if !found {
		t.Error("old record should survive after failed ReplaceByFiles")
	}
}

func TestReplaceByFiles_MultiFile(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	entries := []store.FileRecords{
		{
			FilePath: "/proj/a.go",
			FileHash: "hash-a",
			Records: []store.Record{
				{
					ID:       store.RecordID("/proj/a.go", "func A() {}"),
					FilePath: "/proj/a.go", Language: "go",
					Content: "func A() {}", ContentHash: store.ContentHash([]byte("func A() {}")),
					NodeKind: "function_declaration", Name: "A",
					StartLine: 1, EndLine: 1,
					Embedding: []float32{1, 0, 0},
				},
			},
		},
		{
			FilePath: "/proj/b.go",
			FileHash: "hash-b",
			Records: []store.Record{
				{
					ID:       store.RecordID("/proj/b.go", "func B() {}"),
					FilePath: "/proj/b.go", Language: "go",
					Content: "func B() {}", ContentHash: store.ContentHash([]byte("func B() {}")),
					NodeKind: "function_declaration", Name: "B",
					StartLine: 1, EndLine: 1,
					Embedding: []float32{0, 1, 0},
				},
			},
		},
		{
			FilePath: "/proj/c.go",
			FileHash: "hash-c",
			Records: []store.Record{
				{
					ID:       store.RecordID("/proj/c.go", "func C() {}"),
					FilePath: "/proj/c.go", Language: "go",
					Content: "func C() {}", ContentHash: store.ContentHash([]byte("func C() {}")),
					NodeKind: "function_declaration", Name: "C",
					StartLine: 1, EndLine: 1,
					Embedding: []float32{0, 0, 1},
				},
			},
		},
	}

	if err = s.ReplaceByFiles(ctx, entries); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	files, err := s.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 3 {
		t.Errorf("expected 3 files, got %d", len(files))
	}

	hashes, err := s.FileHashes(ctx, []string{"/proj/a.go", "/proj/b.go", "/proj/c.go"})
	if err != nil {
		t.Fatalf("FileHashes: %v", err)
	}
	if hashes["/proj/a.go"] != "hash-a" {
		t.Errorf("a.go hash = %q; want hash-a", hashes["/proj/a.go"])
	}
	if hashes["/proj/b.go"] != "hash-b" {
		t.Errorf("b.go hash = %q; want hash-b", hashes["/proj/b.go"])
	}
	if hashes["/proj/c.go"] != "hash-c" {
		t.Errorf("c.go hash = %q; want hash-c", hashes["/proj/c.go"])
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.ChunkCount != 3 {
		t.Errorf("chunk count = %d; want 3", stats.ChunkCount)
	}
}

func TestFileHashes_Batch(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	entries := []store.FileRecords{
		{FilePath: "/proj/a.go", FileHash: "hash-a", Records: []store.Record{
			{ID: "id-a", FilePath: "/proj/a.go", Language: "go", Content: "a", ContentHash: "ch-a", NodeKind: "file", StartLine: 1, EndLine: 1, Embedding: []float32{1, 0, 0}},
		}},
		{FilePath: "/proj/b.go", FileHash: "hash-b", Records: []store.Record{
			{ID: "id-b", FilePath: "/proj/b.go", Language: "go", Content: "b", ContentHash: "ch-b", NodeKind: "file", StartLine: 1, EndLine: 1, Embedding: []float32{0, 1, 0}},
		}},
		{FilePath: "/proj/c.go", FileHash: "hash-c", Records: []store.Record{
			{ID: "id-c", FilePath: "/proj/c.go", Language: "go", Content: "c", ContentHash: "ch-c", NodeKind: "file", StartLine: 1, EndLine: 1, Embedding: []float32{0, 0, 1}},
		}},
	}
	if err := s.ReplaceByFiles(ctx, entries); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	// Query for 3 existing + 2 non-existent paths.
	hashes, err := s.FileHashes(ctx, []string{
		"/proj/a.go", "/proj/b.go", "/proj/c.go",
		"/proj/missing1.go", "/proj/missing2.go",
	})
	if err != nil {
		t.Fatalf("FileHashes: %v", err)
	}

	if len(hashes) != 3 {
		t.Errorf("expected 3 entries, got %d", len(hashes))
	}
	if hashes["/proj/a.go"] != "hash-a" {
		t.Errorf("a.go = %q; want hash-a", hashes["/proj/a.go"])
	}
	if hashes["/proj/b.go"] != "hash-b" {
		t.Errorf("b.go = %q; want hash-b", hashes["/proj/b.go"])
	}
	if hashes["/proj/c.go"] != "hash-c" {
		t.Errorf("c.go = %q; want hash-c", hashes["/proj/c.go"])
	}
	if _, ok := hashes["/proj/missing1.go"]; ok {
		t.Error("missing1.go should not be in result")
	}
	if _, ok := hashes["/proj/missing2.go"]; ok {
		t.Error("missing2.go should not be in result")
	}
}

func TestSearch_CombinedFilters(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		{ID: "r1", FilePath: "main.go", Language: "go", Content: "func main() {}", ContentHash: "h1", NodeKind: "function_declaration", Name: "main", StartLine: 1, EndLine: 1, Embedding: []float32{1, 0, 0}},
		{ID: "r2", FilePath: "search.go", Language: "go", Content: "func Search() {}", ContentHash: "h2", NodeKind: "function_declaration", Name: "Search", StartLine: 1, EndLine: 1, Embedding: []float32{0.9, 0.1, 0}},
		{ID: "r3", FilePath: "types.go", Language: "go", Content: "type Server struct{}", ContentHash: "h3", NodeKind: "type_declaration", Name: "Server", StartLine: 1, EndLine: 1, Embedding: []float32{0.8, 0.2, 0}},
		{ID: "r4", FilePath: "search.py", Language: "python", Content: "def search(): pass", ContentHash: "h4", NodeKind: "function_definition", Name: "search", StartLine: 1, EndLine: 1, Embedding: []float32{0.85, 0.15, 0}},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "search",
		Limit:     10,
		Languages: []string{"go"},
		Names:     []string{"Search"},
		NodeKinds: []string{"function_declaration"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "Search" || results[0].Language != "go" || results[0].NodeKind != "function_declaration" {
		t.Errorf("got {Name: %q, Language: %q, NodeKind: %q}, want {Search, go, function_declaration}",
			results[0].Name, results[0].Language, results[0].NodeKind)
	}
}

func TestSQLiteStore_CurrentSchemaVersion(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	v, err := s.Meta(ctx, "schema_version")
	if err != nil {
		t.Fatalf("reading schema_version: %v", err)
	}
	if v != "3" {
		t.Errorf("schema_version = %q, want %q", v, "3")
	}
}

func TestListSymbols_EmptyStore(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	symbols, err := s.ListSymbols(t.Context())
	if err != nil {
		t.Fatalf("ListSymbols: %v", err)
	}
	if len(symbols) != 0 {
		t.Errorf("expected 0 symbols from empty store, got %d", len(symbols))
	}
}

func TestSearch_NameFilter(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		{ID: "r1", FilePath: "main.go", Language: "go", Content: "func main() {}", ContentHash: "h1", NodeKind: "function_declaration", Name: "main", StartLine: 1, EndLine: 1, Embedding: []float32{1, 0, 0}},
		{ID: "r2", FilePath: "search.go", Language: "go", Content: "func Search() {}", ContentHash: "h2", NodeKind: "function_declaration", Name: "Search", StartLine: 1, EndLine: 1, Embedding: []float32{0.9, 0.1, 0}},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "search",
		Limit:     10,
		Names:     []string{"Search"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Name != "Search" {
		t.Errorf("got name %q, want %q", results[0].Name, "Search")
	}
}

func TestSearch_NodeKindFilter(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		{ID: "r1", FilePath: "main.go", Language: "go", Content: "func main() {}", ContentHash: "h1", NodeKind: "function_declaration", Name: "main", StartLine: 1, EndLine: 1, Embedding: []float32{1, 0, 0}},
		{ID: "r2", FilePath: "types.go", Language: "go", Content: "type Server struct{}", ContentHash: "h2", NodeKind: "type_declaration", Name: "Server", StartLine: 1, EndLine: 1, Embedding: []float32{0.9, 0.1, 0}},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "server",
		Limit:     10,
		NodeKinds: []string{"type_declaration"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].NodeKind != "type_declaration" {
		t.Errorf("got node_kind %q, want %q", results[0].NodeKind, "type_declaration")
	}
}

func TestSearch_MetadataOnly(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		{ID: "r1", FilePath: "main.go", Language: "go", Content: "func main() { println(42) }", ContentHash: "h1", NodeKind: "function_declaration", Name: "main", StartLine: 1, EndLine: 1, Embedding: []float32{1, 0, 0}},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding:    []float32{1, 0, 0},
		Text:         "main",
		Limit:        10,
		MetadataOnly: true,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Content != "" {
		t.Errorf("metadata-only should have empty content, got %q", results[0].Content)
	}
	if results[0].Name != "main" {
		t.Errorf("got name %q, want %q", results[0].Name, "main")
	}
	if results[0].FilePath != "main.go" {
		t.Errorf("got file_path %q, want %q", results[0].FilePath, "main.go")
	}
}

func TestSearch_SingleLanguageKNNPreFilter(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := t.Context()
	records := []store.Record{
		{ID: "r1", FilePath: "main.go", Language: "go", Content: "func main() {}", ContentHash: "h1", NodeKind: "function_declaration", Name: "main", StartLine: 1, EndLine: 1, Embedding: []float32{1, 0, 0}},
		{ID: "r2", FilePath: "main.py", Language: "python", Content: "def main(): pass", ContentHash: "h2", NodeKind: "function_definition", Name: "main", StartLine: 1, EndLine: 1, Embedding: []float32{0.99, 0.01, 0}},
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "main",
		Limit:     10,
		Languages: []string{"go"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Language != "go" {
		t.Errorf("got language %q, want %q", results[0].Language, "go")
	}
}

func TestSearch_BatchFetch(t *testing.T) {
	dim := 3
	s, err := store.NewSQLiteStore(testDB(t), "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	records := make([]store.Record, 20)
	for i := range 20 {
		id := fmt.Sprintf("r%02d", i)
		records[i] = store.Record{
			ID: id, FilePath: fmt.Sprintf("/src/file%d.go", i), Language: "go",
			Content: fmt.Sprintf("func f%d() {}", i), ContentHash: fmt.Sprintf("h%d", i),
			NodeKind: "function", Name: fmt.Sprintf("f%d", i),
			StartLine: 1, EndLine: 3,
			Embedding: make([]float32, dim),
		}
		records[i].Embedding[i%dim] = 1.0
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "func",
		Limit:     5,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected search results")
	}
	if len(results) > 5 {
		t.Errorf("expected at most 5 results, got %d", len(results))
	}

	for _, r := range results {
		if r.Content == "" {
			t.Errorf("result %q has empty Content — batch fetch may be broken", r.ID)
		}
		if r.Language == "" {
			t.Errorf("result %q has empty Language", r.ID)
		}
	}
}

func TestSearch_IterativeDeepening(t *testing.T) {
	dbPath := testDB(t)
	s, err := store.NewSQLiteStore(dbPath, "test-model", 3)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ctx := t.Context()

	// Insert 10 records: 2 Go, 8 Python. All with similar embeddings.
	var records []store.Record
	for i := range 10 {
		lang := "python"
		if i < 2 {
			lang = "go"
		}
		records = append(records, store.Record{
			ID:          fmt.Sprintf("r%d", i),
			FilePath:    fmt.Sprintf("file%d.%s", i, lang),
			Language:    lang,
			Content:     fmt.Sprintf("content %d", i),
			ContentHash: fmt.Sprintf("h%d", i),
			NodeKind:    "function_declaration",
			Name:        fmt.Sprintf("func%d", i),
			StartLine:   1,
			EndLine:     1,
			Embedding:   []float32{1, 0, 0},
		})
	}
	if err = s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Request 2 Go results. With initial fetchLimit=6, KNN might return
	// mostly Python results. Iterative deepening should widen the search.
	results, err := s.Search(ctx, store.SearchQuery{
		Embedding: []float32{1, 0, 0},
		Text:      "content",
		Limit:     2,
		Languages: []string{"go"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	for _, r := range results {
		if r.Language != "go" {
			t.Errorf("got language %q, want %q", r.Language, "go")
		}
	}
}

func TestEmbeddingsByContentHash_Deterministic(t *testing.T) {
	s, err := store.NewSQLiteStore(testDB(t), "test-model", 3)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer s.Close()

	ctx := t.Context()

	// Insert two chunks with the same content hash but different IDs/embeddings.
	// DISTINCT should return a deterministic result (one row per content_hash).
	contentHash := store.ContentHash([]byte("func shared() {}"))
	r1 := store.Record{
		ID: "dup1", FilePath: "a.go", Language: "go",
		Content: "func shared() {}", ContentHash: contentHash,
		NodeKind: "function_declaration", Name: "shared",
		StartLine: 1, EndLine: 1,
		Embedding: []float32{0.1, 0.2, 0.3},
	}
	r2 := store.Record{
		ID: "dup2", FilePath: "b.go", Language: "go",
		Content: "func shared() {}", ContentHash: contentHash,
		NodeKind: "function_declaration", Name: "shared",
		StartLine: 1, EndLine: 1,
		Embedding: []float32{0.4, 0.5, 0.6},
	}

	if err = s.Upsert(ctx, []store.Record{r1, r2}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// Query multiple times to verify deterministic results.
	var firstResult []float32
	for i := range 5 {
		embeddings, err := s.EmbeddingsByContentHash(ctx, []string{contentHash})
		if err != nil {
			t.Fatalf("iteration %d: EmbeddingsByContentHash: %v", i, err)
		}
		if len(embeddings) != 1 {
			t.Fatalf("iteration %d: expected 1 embedding, got %d", i, len(embeddings))
		}
		emb := embeddings[contentHash]
		if firstResult == nil {
			firstResult = emb
		} else {
			for j := range emb {
				if emb[j] != firstResult[j] {
					t.Errorf("iteration %d: embedding[%d] = %v, want %v (non-deterministic)", i, j, emb[j], firstResult[j])
				}
			}
		}
	}
}

func TestConcurrentReadWrite(t *testing.T) {
	dbPath := t.TempDir() + "/concurrent.db"
	dim := 3
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Seed initial data.
	seed := []store.Record{{
		ID: "seed-1", FilePath: "seed.go", Language: "go",
		Content: "func seed() {}", ContentHash: "seedhash", NodeKind: "function",
		Name: "seed", StartLine: 1, EndLine: 1,
		Embedding: []float32{1, 0, 0},
	}}
	if err = s.Upsert(ctx, seed); err != nil {
		t.Fatalf("seed upsert: %v", err)
	}

	var wg sync.WaitGroup
	var errCount atomic.Int64
	const writers = 3
	const readers = 5
	const iterations = 20

	// Writer goroutines.
	for w := range writers {
		wg.Go(func() {
			for i := range iterations {
				id := fmt.Sprintf("w%d-r%d", w, i)
				rec := []store.Record{{
					ID: id, FilePath: fmt.Sprintf("w%d.go", w), Language: "go",
					Content: "func f() {}", ContentHash: id, NodeKind: "function",
					Name: "f", StartLine: 1, EndLine: 1,
					Embedding: []float32{1, 0, 0},
				}}
				if err := s.ReplaceByFiles(ctx, []store.FileRecords{{FilePath: fmt.Sprintf("w%d.go", w), Records: rec, FileHash: id}}); err != nil {
					errCount.Add(1)
					t.Errorf("writer %d iteration %d: %v", w, i, err)
				}
			}
		})
	}

	// Reader goroutines.
	for range readers {
		wg.Go(func() {
			for range iterations {
				// err is declared per goroutine; assigning the outer err would
				// be a data race between readers.
				if _, err := s.Search(ctx, store.SearchQuery{
					Embedding: []float32{1, 0, 0},
					Text:      "func",
					Limit:     5,
				}); err != nil {
					errCount.Add(1)
					t.Errorf("reader search: %v", err)
				}
			}
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		if n := errCount.Load(); n > 0 {
			t.Errorf("got %d errors from concurrent writes", n)
		}
	case <-ctx.Done():
		t.Fatal("deadlock: concurrent read/write did not complete within timeout")
	}
}

func TestFileStates(t *testing.T) {
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := t.Context()

	// Absent path: omitted from the result.
	states, err := s.FileStates(ctx, []string{"missing.go"})
	if err != nil {
		t.Fatalf("FileStates not-found: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("expected no states for absent path, got %v", states)
	}

	// Write a file record carrying mtime + size.
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{
		{FilePath: "main.go", FileHash: "abc123", Mtime: 1700000000, Size: 4096},
	}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	states, err = s.FileStates(ctx, []string{"main.go"})
	if err != nil {
		t.Fatalf("FileStates: %v", err)
	}
	got := states["main.go"]
	if got.ContentHash != "abc123" || got.Mtime != 1700000000 || got.Size != 4096 {
		t.Errorf("FileStates = %+v, want {abc123 1700000000 4096}", got)
	}
}
