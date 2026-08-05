package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ieshan/codamigo/mcp"
	"github.com/ieshan/codamigo/query"
	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/codamigo/watcher"
)

func TestNewServer_NilDependencies(t *testing.T) {
	s := mcp.NewServer(nil, nil, nil)
	if s == nil {
		t.Fatal("NewServer returned nil")
	}
}

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

func (f *fakeEmbedder) Dim() int { return len(f.vec) }

func setupTestServer(t *testing.T) *mcp.Server {
	t.Helper()
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := t.Context()
	records := []store.Record{
		{
			ID: "r1", FilePath: "src/main.go", Language: "go",
			Content: "func main() {}", ContentHash: "h1", NodeKind: "function",
			Name: "main", StartLine: 1, EndLine: 3,
			Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "src/utils/helper.go", Language: "go",
			Content: "func helper() int { return 42 }", ContentHash: "h2", NodeKind: "function",
			Name: "helper", StartLine: 1, EndLine: 5,
			Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)
	return mcp.NewServer(q, nil, nil, mcp.WithNonCodeLanguages([]string{"markdown", "yaml", "json"}))
}

func TestHandleSearch_BasicQuery(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query: "main function",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}

	text := result.Content[0].(*mcpsdk.TextContent).Text
	var resp struct {
		Results   []query.Result `json:"results"`
		Truncated bool           `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected at least one result")
	}
	if resp.Truncated {
		t.Error("expected Truncated=false for unlimited search")
	}
}

func TestHandleSearch_MissingQuery(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing query")
	}
}

func TestHandleSearch_NoQuerier(t *testing.T) {
	srv := mcp.NewServer(nil, nil, nil)
	ctx := t.Context()

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query: "test",
	})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result when no querier configured")
	}
}

func TestHandleMap_BasicOutput(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleMap(ctx, &mcpsdk.CallToolRequest{}, mcp.MapInput{})
	if err != nil {
		t.Fatalf("handleMap: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}

	text := result.Content[0].(*mcpsdk.TextContent).Text
	if text == "" {
		t.Fatal("expected non-empty map output")
	}
	if !strings.Contains(text, "# package:") {
		t.Error("expected package header in map output")
	}
}

func TestHandleMap_DefaultMaxTokens(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleMap(ctx, &mcpsdk.CallToolRequest{}, mcp.MapInput{})
	if err != nil {
		t.Fatalf("handleMap: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}
	text := result.Content[0].(*mcpsdk.TextContent).Text
	if strings.Contains(text, "truncated") {
		t.Error("small test data should not be truncated at default budget")
	}
}

func TestHandleMap_NoQuerier(t *testing.T) {
	srv := mcp.NewServer(nil, nil, nil)
	ctx := t.Context()

	result, _, err := srv.HandleMap(ctx, &mcpsdk.CallToolRequest{}, mcp.MapInput{})
	if err != nil {
		t.Fatalf("handleMap: %v", err)
	}
	if !result.IsError {
		t.Error("expected error when no querier configured")
	}
}

func TestHandleSearch_MaxTokens(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:     "function",
		Limit:     10,
		MaxTokens: 1,
	})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}

	text := result.Content[0].(*mcpsdk.TextContent).Text
	var resp struct {
		Results   []query.Result `json:"results"`
		Truncated bool           `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Error("expected at least one result even with tiny budget")
	}
}

// setupTestServerWithRecords creates a server seeded with n records so that
// limit-clamping tests can observe non-empty result sets.
func setupTestServerWithRecords(t *testing.T, n int) *mcp.Server {
	t.Helper()
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	st, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	records := make([]store.Record, n)
	for i := range n {
		records[i] = store.Record{
			ID:          fmt.Sprintf("r%d", i),
			FilePath:    fmt.Sprintf("src/file%d.go", i),
			Language:    "go",
			Content:     fmt.Sprintf("func fn%d() {}", i),
			ContentHash: fmt.Sprintf("h%d", i),
			NodeKind:    "function",
			Name:        fmt.Sprintf("fn%d", i),
			StartLine:   1,
			EndLine:     3,
			// All embeddings point in the same direction so every record
			// is a near-match for the fakeEmbedder vector {1, 0, 0}.
			Embedding: []float32{1, 0, 0},
		}
	}
	ctx := t.Context()
	if err := st.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, st)
	return mcp.NewServer(q, nil, nil, mcp.WithNonCodeLanguages([]string{"markdown", "yaml", "json"}))
}

func TestHandleSearch_LimitClamping(t *testing.T) {
	// Seed more records than the maximum allowed limit (100) so that an
	// unclamped request would actually return more than 100 results.
	srv := setupTestServerWithRecords(t, 150)
	ctx := t.Context()

	cases := []struct {
		name    string
		limit   int
		wantMax int
	}{
		{"negative", -5, 10},    // clamped to default 10
		{"zero", 0, 10},         // clamped to default 10
		{"valid", 20, 20},       // respected as-is
		{"above_max", 500, 100}, // clamped to hard max 100
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
				Query: "function",
				Limit: tc.limit,
			})
			if err != nil {
				t.Fatalf("HandleSearch: %v", err)
			}
			if result.IsError {
				t.Fatalf("unexpected error result: %v", result.Content)
			}
			text := result.Content[0].(*mcpsdk.TextContent).Text
			var resp struct {
				Results []query.Result `json:"results"`
			}
			if err := json.Unmarshal([]byte(text), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(resp.Results) > tc.wantMax {
				t.Errorf("limit=%d: got %d results, want <= %d (clamping failed)",
					tc.limit, len(resp.Results), tc.wantMax)
			}
		})
	}
}

func TestHandleSearch_NegativeOffsetAndMaxTokens(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:     "function",
		Limit:     10,
		Offset:    -3,
		MaxTokens: -100,
	})
	if err != nil {
		t.Fatalf("HandleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
}

func TestHandleSearch_RefreshCooldown(t *testing.T) {
	// Build a server with a mock indexer that counts how many times Index is called.
	dim := 3
	dbPath := t.TempDir() + "/cooldown.db"
	st, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, st)

	calls := 0
	mock := &mockIndexer{indexFn: func(_ context.Context) error {
		calls++
		return nil
	}}

	srv := mcp.NewServerWithIndexer(q, mock, nil)
	ctx := t.Context()

	// First call — cooldown not yet set; indexer should be called.
	_, _, err = srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:        "function",
		RefreshIndex: true,
	})
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected indexer called once after first refresh, got %d", calls)
	}

	// Immediately call again — still within cooldown; indexer must NOT be called.
	_, _, err = srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:        "function",
		RefreshIndex: true,
	})
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected indexer still called once (cooldown), got %d", calls)
	}
}

// mockIndexer is a test double for the Indexer interface used by the MCP server.
type mockIndexer struct {
	indexFn      func(ctx context.Context) error
	indexFilesFn func(ctx context.Context, paths []string) error
	staleFn      func(ctx context.Context, paths []string, stored map[string]store.FileState) (map[string]struct{}, error)
	indexCalled  bool
	filesCalled  bool
}

func (m *mockIndexer) Index(ctx context.Context) error {
	m.indexCalled = true
	if m.indexFn != nil {
		return m.indexFn(ctx)
	}
	return nil
}

func (m *mockIndexer) IndexFiles(ctx context.Context, paths []string) error {
	m.filesCalled = true
	if m.indexFilesFn != nil {
		return m.indexFilesFn(ctx, paths)
	}
	return nil
}

func (m *mockIndexer) StaleFiles(ctx context.Context, paths []string, stored map[string]store.FileState) (map[string]struct{}, error) {
	if m.staleFn != nil {
		return m.staleFn(ctx, paths, stored)
	}
	return map[string]struct{}{}, nil
}

// seedFile writes one single-chunk file into the store via ReplaceByFiles so
// the files table (stored content hashes) is populated for staleness tests.
func seedFile(t *testing.T, st store.Store, path, fileHash, content string) {
	t.Helper()
	rec := store.Record{
		ID:          store.RecordID(path, content),
		FilePath:    path,
		Language:    "go",
		Content:     content,
		ContentHash: store.ContentHash([]byte(content)),
		NodeKind:    "function",
		Name:        "fn",
		StartLine:   1,
		EndLine:     1,
		Embedding:   []float32{1, 0, 0},
	}
	if err := st.ReplaceByFiles(t.Context(), []store.FileRecords{
		{FilePath: path, FileHash: fileHash, Records: []store.Record{rec}},
	}); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func decodeResults(t *testing.T, result *mcpsdk.CallToolResult) []query.Result {
	t.Helper()
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}
	text := result.Content[0].(*mcpsdk.TextContent).Text
	var resp struct {
		Results []query.Result `json:"results"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal results: %v", err)
	}
	return resp.Results
}

func TestHandleSearch_StaleRefreshInPlace(t *testing.T) {
	st, err := store.NewSQLiteStore(t.TempDir()+"/s.db", "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()

	seedFile(t, st, "a.go", "oldhash", "OLD content")

	q := query.New(&fakeEmbedder{vec: []float32{1, 0, 0}}, st)
	reindexed := false
	mock := &mockIndexer{
		// a.go reads as stale until it is re-indexed in place.
		staleFn: func(_ context.Context, _ []string, _ map[string]store.FileState) (map[string]struct{}, error) {
			if reindexed {
				return map[string]struct{}{}, nil
			}
			return map[string]struct{}{"a.go": {}}, nil
		},
		// Simulate a real re-index: replace a.go's chunk with fresh content.
		indexFilesFn: func(_ context.Context, _ []string) error {
			reindexed = true
			seedFile(t, st, "a.go", "newhash", "NEW content")
			return nil
		},
	}
	srv := mcp.NewServerWithIndexer(q, mock, nil)

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{Query: "fn", Limit: 10})
	if err != nil {
		t.Fatalf("HandleSearch: %v", err)
	}

	if !mock.filesCalled {
		t.Error("expected Tier 1 in-place re-index (IndexFiles) to be called")
	}
	results := decodeResults(t, result)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Content != "NEW content" {
		t.Errorf("expected refreshed content %q, got %q", "NEW content", results[0].Content)
	}
	if results[0].Stale {
		t.Error("result should not be flagged stale after a successful refresh")
	}
}

func TestHandleSearch_ManyStaleFlaggedNotRefreshed(t *testing.T) {
	st, err := store.NewSQLiteStore(t.TempDir()+"/s.db", "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()

	const n = 15 // exceeds staleRefreshThreshold (10)
	for i := range n {
		seedFile(t, st, fmt.Sprintf("f%d.go", i), fmt.Sprintf("old-%d", i), fmt.Sprintf("content %d", i))
	}

	q := query.New(&fakeEmbedder{vec: []float32{1, 0, 0}}, st)
	mock := &mockIndexer{
		// Every result file reads as stale.
		staleFn: func(_ context.Context, paths []string, _ map[string]store.FileState) (map[string]struct{}, error) {
			m := make(map[string]struct{}, len(paths))
			for _, p := range paths {
				m[p] = struct{}{}
			}
			return m, nil
		},
	}
	srv := mcp.NewServerWithIndexer(q, mock, nil)

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{Query: "fn", Limit: 20})
	if err != nil {
		t.Fatalf("HandleSearch: %v", err)
	}

	if mock.filesCalled {
		t.Error("should not re-index in place when stale count exceeds threshold")
	}
	results := decodeResults(t, result)
	if len(results) != n {
		t.Fatalf("expected %d results, got %d", n, len(results))
	}
	for _, r := range results {
		if !r.Stale {
			t.Errorf("expected result %s to be flagged stale", r.FilePath)
		}
	}
}

func TestHandleSearch_FreshResultsNotFlagged(t *testing.T) {
	st, err := store.NewSQLiteStore(t.TempDir()+"/s.db", "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()

	seedFile(t, st, "a.go", "samehash", "content")

	q := query.New(&fakeEmbedder{vec: []float32{1, 0, 0}}, st)
	mock := &mockIndexer{
		// Nothing reads as stale.
		staleFn: func(_ context.Context, _ []string, _ map[string]store.FileState) (map[string]struct{}, error) {
			return map[string]struct{}{}, nil
		},
	}
	srv := mcp.NewServerWithIndexer(q, mock, nil)

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{Query: "fn", Limit: 10})
	if err != nil {
		t.Fatalf("HandleSearch: %v", err)
	}

	if mock.filesCalled {
		t.Error("should not re-index when nothing is stale")
	}
	results := decodeResults(t, result)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Stale {
		t.Error("fresh result must not be flagged stale")
	}
}

func TestHandleSearch_PackageFilter(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:   "function",
		Limit:   10,
		Package: "src/utils",
	})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}

	text := result.Content[0].(*mcpsdk.TextContent).Text
	var resp struct {
		Results   []query.Result `json:"results"`
		Truncated bool           `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Verify filtering actually worked: every returned result must be within
	// the requested package directory, and src/main.go must be excluded.
	for _, r := range resp.Results {
		if r.FilePath == "src/main.go" {
			t.Error("package filter should exclude src/main.go")
		}
		if !strings.HasPrefix(r.FilePath, "src/utils") {
			t.Errorf("package filter returned result outside src/utils: %s", r.FilePath)
		}
	}
	// The only record in src/utils is helper.go; expect exactly one result.
	if len(resp.Results) != 1 {
		t.Errorf("expected 1 result for package src/utils, got %d", len(resp.Results))
	}
}

func TestHandleSearch_MetadataOnly(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:        "function",
		Limit:        10,
		MetadataOnly: true,
	})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}

	text := result.Content[0].(*mcpsdk.TextContent).Text
	if text == "" {
		t.Fatal("expected non-empty metadata_only response")
	}

	// metadata_only must not return JSON.
	if strings.HasPrefix(strings.TrimSpace(text), "{") {
		t.Error("metadata_only response should be plain text, not JSON")
	}

	// Each line must follow the format: "file:line  name                 nodekind"
	// (server.go: fmt.Fprintf(&b, "%s:%d  %-20s %s\n", r.FilePath, r.StartLine, r.Name, r.NodeKind))
	// Verify the two seeded records appear with correct filepath:line references.
	if !strings.Contains(text, "src/main.go:1") {
		t.Error("metadata_only response should contain 'src/main.go:1'")
	}
	if !strings.Contains(text, "src/utils/helper.go:1") {
		t.Error("metadata_only response should contain 'src/utils/helper.go:1'")
	}

	// Node kind must be present; source content must be absent.
	if !strings.Contains(text, "function") {
		t.Error("metadata_only response should contain node kind 'function'")
	}
	if strings.Contains(text, "func main()") || strings.Contains(text, "func helper()") {
		t.Error("metadata_only response must not contain source content")
	}
}

func TestHandleSearch_PackagePathTraversal(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:   "test",
		Package: "../../../etc",
	})
	if err != nil {
		t.Fatalf("handleSearch: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error for path traversal in package parameter, got success")
	}
	// Error message should mention ".." so callers understand the rejection.
	text := result.Content[0].(*mcpsdk.TextContent).Text
	if !strings.Contains(text, "..") {
		t.Errorf("error message should mention '..', got: %s", text)
	}

	// Test the filepath.Clean bypass pattern: no ".." substring but resolves outside root.
	result2, _, err2 := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:   "test",
		Package: "store/./../../etc",
	})
	if err2 != nil {
		t.Fatalf("handleSearch: %v", err2)
	}
	if !result2.IsError {
		t.Fatal("expected error for filepath.Clean bypass pattern, got success")
	}

	// "store/.." cleans to "." which would match everything via "./**" glob.
	result3, _, err3 := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:   "test",
		Package: "store/..",
	})
	if err3 != nil {
		t.Fatalf("handleSearch: %v", err3)
	}
	if !result3.IsError {
		t.Fatal("expected error for package 'store/..' (cleans to '.'), got success")
	}

	// Bare "." should also be rejected.
	result4, _, err4 := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
		Query:   "test",
		Package: ".",
	})
	if err4 != nil {
		t.Fatalf("handleSearch: %v", err4)
	}
	if !result4.IsError {
		t.Fatal("expected error for package '.', got success")
	}
}

func TestHandleSearch_UnsupportedDoubleStarGlob(t *testing.T) {
	srv := setupTestServer(t)
	ctx := t.Context()

	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"mid-pattern double star", "src/**/test.go", true},
		{"leading double star", "**/foo.go", true},
		{"trailing dir slash ok", "src/**", false},
		{"trailing dir glob ok", "src/utils/**", false},
		{"single star ok", "src/*.go", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := srv.HandleSearch(ctx, &mcpsdk.CallToolRequest{}, mcp.SearchInput{
				Query: "function",
				Limit: 10,
				Paths: []string{tc.path},
			})
			if err != nil {
				t.Fatalf("HandleSearch: %v", err)
			}
			if tc.wantErr && !result.IsError {
				t.Errorf("expected error for path %q, got success", tc.path)
			}
			if !tc.wantErr && result.IsError {
				t.Errorf("unexpected error for path %q: %v", tc.path, result.Content)
			}
		})
	}
}

func setupTestServerWithMarkdown(t *testing.T) *mcp.Server {
	t.Helper()
	dim := 3
	dbPath := t.TempDir() + "/test.db"
	s, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := t.Context()
	records := []store.Record{
		{
			ID: "r1", FilePath: "src/main.go", Language: "go",
			Content: "func main() {}", ContentHash: "h1", NodeKind: "function_declaration",
			Name: "main", StartLine: 1, EndLine: 3,
			Embedding: []float32{1, 0, 0},
		},
		{
			ID: "r2", FilePath: "CHANGELOG.md", Language: "markdown",
			Content: "# v1.0", ContentHash: "h2", NodeKind: "atx_heading",
			Name: "v1.0", StartLine: 1, EndLine: 1,
			Embedding: []float32{0, 1, 0},
		},
	}
	if err := s.Upsert(ctx, records); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, s)
	return mcp.NewServer(q, nil, nil, mcp.WithNonCodeLanguages([]string{"markdown", "yaml", "json"}))
}

func TestHandleMap_CodeOnlyParam(t *testing.T) {
	srv := setupTestServerWithMarkdown(t)
	ctx := t.Context()

	// Default (code_only=true): markdown excluded.
	result, _, err := srv.HandleMap(ctx, &mcpsdk.CallToolRequest{}, mcp.MapInput{})
	if err != nil {
		t.Fatalf("HandleMap: %v", err)
	}
	text := result.Content[0].(*mcpsdk.TextContent).Text
	if strings.Contains(text, "CHANGELOG") {
		t.Error("default code_only=true should exclude markdown files")
	}
	if !strings.Contains(text, "main") {
		t.Error("expected Go symbol in output")
	}

	// Explicit code_only=false: markdown included.
	result2, _, err := srv.HandleMap(ctx, &mcpsdk.CallToolRequest{}, mcp.MapInput{
		CodeOnly: new(false),
	})
	if err != nil {
		t.Fatalf("HandleMap: %v", err)
	}
	text2 := result2.Content[0].(*mcpsdk.TextContent).Text
	if !strings.Contains(text2, "CHANGELOG") {
		t.Error("code_only=false should include markdown files")
	}
}

// testWatcher implements watcher.Watcher for testing watchLoop.
type testWatcher struct {
	ch chan []watcher.Event
}

func (t *testWatcher) Watch(_ context.Context) <-chan []watcher.Event { return t.ch }
func (t *testWatcher) Close() error                                   { return nil }

func TestWatchLoop_ReindexTriggersFullIndex(t *testing.T) {
	dim := 3
	dbPath := t.TempDir() + "/reindex.db"
	st, err := store.NewSQLiteStore(dbPath, "test-model", dim)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	emb := &fakeEmbedder{vec: []float32{1, 0, 0}}
	q := query.New(emb, st)

	var indexCount int
	var mu sync.Mutex
	mock := &mockIndexer{
		indexFn: func(_ context.Context) error {
			mu.Lock()
			indexCount++
			mu.Unlock()
			return nil
		},
	}
	tw := &testWatcher{ch: make(chan []watcher.Event, 1)}

	srv := mcp.NewServerWithIndexer(q, mock, tw)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Use a pipe so ServeIO blocks on reading (MCP loop waits for input).
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	defer func() { _ = r.Close() }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = srv.ServeIO(ctx, r, io.Discard)
	}()

	// Wait for the initial index to complete (ServeIO calls Index once).
	time.Sleep(200 * time.Millisecond)

	// Send a Reindex event through the watcher channel.
	tw.ch <- []watcher.Event{{Op: watcher.Reindex}}

	// Give watchLoop time to process the event.
	time.Sleep(200 * time.Millisecond)

	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	// Initial index = 1 call, Reindex event = 1 call → total 2.
	if indexCount != 2 {
		t.Errorf("expected Index called 2 times (initial + reindex), got %d", indexCount)
	}
	if mock.filesCalled {
		t.Error("IndexFiles should not be called when Reindex event is received")
	}
}
