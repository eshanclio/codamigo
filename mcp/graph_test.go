package mcp_test

import (
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ieshan/codamigo/mcp"
	"github.com/ieshan/codamigo/query"
	"github.com/ieshan/codamigo/store"
)

// setupGraphServer builds a server over a two-file index with a call edge from
// Run (main.go) to Helper (helper.go).
func setupGraphServer(t *testing.T, opts ...mcp.Option) *mcp.Server {
	t.Helper()
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	rec := func(id, path, name string, start, end int) store.Record {
		return store.Record{
			ID: id, FilePath: path, Language: "go",
			Content: "body of " + name, ContentHash: "h-" + id,
			NodeKind: "function_declaration", Name: name,
			StartLine: start, EndLine: end,
			Embedding: []float32{1, 0, 0},
		}
	}

	if err = s.ReplaceByFiles(t.Context(), []store.FileRecords{
		{
			FilePath: "main.go", FileHash: "h1",
			Records: []store.Record{rec("m1", "main.go", "Run", 1, 10)},
			Edges: []store.Edge{
				{SrcID: "m1", Kind: "call", DstName: "Helper", Line: 5},
				{SrcID: "m1", Kind: "call", DstName: "Println", DstQualifier: "fmt", Line: 6},
			},
		},
		{
			FilePath: "helper.go", FileHash: "h2",
			Records: []store.Record{rec("h1c", "helper.go", "Helper", 1, 8)},
		},
	}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}

	q := query.New(&fakeEmbedder{vec: []float32{1, 0, 0}}, s)
	return mcp.NewServer(q, nil, nil, opts...)
}

// listToolNames connects a real MCP client over an in-memory transport and
// returns the advertised tools by name, so tests assert what an agent would
// actually see rather than an internal registry.
func listToolNames(t *testing.T, srv *mcp.Server) map[string]*mcpsdk.Tool {
	t.Helper()
	ctx := t.Context()

	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.ServeTransport(ctx, serverTransport)
	}()
	t.Cleanup(func() { <-done })

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	tools := make(map[string]*mcpsdk.Tool, len(res.Tools))
	for _, tool := range res.Tools {
		tools[tool.Name] = tool
	}
	return tools
}

func resultText(t *testing.T, result *mcpsdk.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("expected content in result")
	}
	tc, ok := result.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("expected text content, got %T", result.Content[0])
	}
	return tc.Text
}

func TestHandleGetCallers(t *testing.T) {
	srv := setupGraphServer(t)

	result, _, err := srv.HandleGetCallers(t.Context(), &mcpsdk.CallToolRequest{}, mcp.GetCallersInput{
		Symbol: "Helper",
	})
	if err != nil {
		t.Fatalf("HandleGetCallers: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "Run") {
		t.Errorf("expected Run in output, got:\n%s", text)
	}
	if !strings.Contains(text, "main.go") {
		t.Errorf("expected the caller's file in output, got:\n%s", text)
	}
}

func TestHandleGetCallees(t *testing.T) {
	srv := setupGraphServer(t)

	result, _, err := srv.HandleGetCallees(t.Context(), &mcpsdk.CallToolRequest{}, mcp.GetCalleesInput{
		Symbol: "Run",
	})
	if err != nil {
		t.Fatalf("HandleGetCallees: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "Helper") {
		t.Errorf("expected Helper in output, got:\n%s", text)
	}
	// An unresolved third-party target is reported and marked.
	if !strings.Contains(text, "fmt.Println") || !strings.Contains(text, "external") {
		t.Errorf("expected fmt.Println marked external, got:\n%s", text)
	}
}

func TestHandleGetImpact(t *testing.T) {
	srv := setupGraphServer(t)

	result, _, err := srv.HandleGetImpact(t.Context(), &mcpsdk.CallToolRequest{}, mcp.GetImpactInput{
		Symbol: "Helper",
		Depth:  2,
	})
	if err != nil {
		t.Fatalf("HandleGetImpact: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %v", result.Content)
	}

	text := resultText(t, result)
	if !strings.Contains(text, "Run") {
		t.Errorf("expected Run in impact output, got:\n%s", text)
	}
	if !strings.Contains(text, "depth=") {
		t.Errorf("impact output should report traversal depth, got:\n%s", text)
	}
}

func TestHandleGraph_MissingSymbol(t *testing.T) {
	srv := setupGraphServer(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name string
		call func() (*mcpsdk.CallToolResult, any, error)
	}{
		{"get_callers", func() (*mcpsdk.CallToolResult, any, error) {
			return srv.HandleGetCallers(ctx, &mcpsdk.CallToolRequest{}, mcp.GetCallersInput{})
		}},
		{"get_callees", func() (*mcpsdk.CallToolResult, any, error) {
			return srv.HandleGetCallees(ctx, &mcpsdk.CallToolRequest{}, mcp.GetCalleesInput{})
		}},
		{"get_impact", func() (*mcpsdk.CallToolResult, any, error) {
			return srv.HandleGetImpact(ctx, &mcpsdk.CallToolRequest{}, mcp.GetImpactInput{})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := tc.call()
			if err != nil {
				t.Fatalf("handler returned a transport error: %v", err)
			}
			if !result.IsError {
				t.Error("empty symbol should produce an error result")
			}
		})
	}
}

// A symbol with no relationships is a normal answer, not an error.
func TestHandleGetCallers_NoResults(t *testing.T) {
	srv := setupGraphServer(t)

	result, _, err := srv.HandleGetCallers(t.Context(), &mcpsdk.CallToolRequest{}, mcp.GetCallersInput{
		Symbol: "Run",
	})
	if err != nil {
		t.Fatalf("HandleGetCallers: %v", err)
	}
	if result.IsError {
		t.Fatalf("no callers should not be an error: %v", result.Content)
	}
	if text := resultText(t, result); !strings.Contains(text, "No callers") {
		t.Errorf("expected a no-callers message, got:\n%s", text)
	}
}

// When the index was built with the graph disabled, the agent gets an actionable
// message rather than a misleading empty answer.
func TestHandleGetCallers_GraphNotBuilt(t *testing.T) {
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err = s.Upsert(t.Context(), []store.Record{{
		ID: "r1", FilePath: "a.go", Language: "go",
		Content: "func A() {}", ContentHash: "h1", NodeKind: "function_declaration",
		Name: "A", StartLine: 1, EndLine: 3, Embedding: []float32{1, 0, 0},
	}}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	srv := mcp.NewServer(query.New(&fakeEmbedder{vec: []float32{1, 0, 0}}, s), nil, nil)

	result, _, err := srv.HandleGetCallers(t.Context(), &mcpsdk.CallToolRequest{}, mcp.GetCallersInput{
		Symbol: "A",
	})
	if err != nil {
		t.Fatalf("HandleGetCallers: %v", err)
	}
	if result.IsError {
		t.Fatalf("a missing graph is actionable, not an error: %v", result.Content)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "not been built") {
		t.Errorf("expected a graph-not-built message, got:\n%s", text)
	}
}

// The limit parameter is clamped and truncation is reported.
func TestHandleGetCallees_LimitClamped(t *testing.T) {
	srv := setupGraphServer(t)

	result, _, err := srv.HandleGetCallees(t.Context(), &mcpsdk.CallToolRequest{}, mcp.GetCalleesInput{
		Symbol: "Run",
		Limit:  1,
	})
	if err != nil {
		t.Fatalf("HandleGetCallees: %v", err)
	}
	text := resultText(t, result)
	if !strings.Contains(text, "truncated") {
		t.Errorf("expected truncation notice at limit 1, got:\n%s", text)
	}
}

// Graph tools must not be advertised when the feature is off.
func TestGraphTools_HiddenWhenDisabled(t *testing.T) {
	srv := setupGraphServer(t, mcp.WithGraph(false))

	tools := listToolNames(t, srv)
	for _, name := range []string{"get_callers", "get_callees", "get_impact"} {
		if _, ok := tools[name]; ok {
			t.Errorf("%s should not be advertised when the graph is disabled", name)
		}
	}
	if _, ok := tools["search"]; !ok {
		t.Error("search must still be advertised")
	}
}

func TestGraphTools_AdvertisedWithAnnotations(t *testing.T) {
	srv := setupGraphServer(t)

	tools := listToolNames(t, srv)
	for _, name := range []string{"get_callers", "get_callees", "get_impact"} {
		tool, ok := tools[name]
		if !ok {
			t.Errorf("%s should be advertised", name)
			continue
		}
		if tool.Title == "" {
			t.Errorf("%s should declare a human-readable Title", name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s should be annotated read-only", name)
		}
	}
}
