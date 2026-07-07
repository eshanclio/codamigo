// Package mcp exposes codamigo's search capability as an MCP (Model Context
// Protocol) stdio server.
//
// [Server] is constructed with a [*query.Querier] and optional
// [*indexer.Indexer] and [watcher.Watcher]. [Server.Serve] runs the MCP
// stdio loop until the context is cancelled. The server exposes a "search"
// tool for semantic code search and a "get_map" tool that returns a
// structural map of the codebase. When no Indexer is provided,
// refresh_index=true is silently ignored.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ieshan/codamigo/indexer"
	"github.com/ieshan/codamigo/query"
	"github.com/ieshan/codamigo/watcher"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// refreshCooldown is the minimum duration between forced re-indexes triggered
// by refresh_index=true. A full index takes ~5–8 s; 30 s prevents hammering.
const refreshCooldown = 30 * time.Second

// indexerIface is the subset of [*indexer.Indexer] used by Server.
// Defined here (consumer package) so tests can substitute a mock without
// importing the real indexer.
type indexerIface interface {
	Index(ctx context.Context) error
	IndexFiles(ctx context.Context, paths []string) error
}

// Server is the MCP stdio server for codamigo.
type Server struct {
	querier          *query.Querier
	indexer          indexerIface
	watcher          watcher.Watcher
	nonCodeLanguages []string
	indexMu          sync.Mutex
	lastRefresh      time.Time // guarded by indexMu
}

// NewServer constructs the MCP server.
// All dependencies are optional (may be nil); the server degrades gracefully.
// When idx is non-nil, a search request with refresh_index=true triggers a
// full re-index before querying. When w is non-nil, changed files are
// re-indexed continuously in the background. nonCodeLangs configures which
// languages are excluded when the get_map code_only option is true.
func NewServer(q *query.Querier, idx *indexer.Indexer, w watcher.Watcher, nonCodeLangs []string) *Server {
	// Guard against the Go interface nil trap: a nil *indexer.Indexer stored in
	// an interface becomes a non-nil interface value, breaking s.indexer != nil
	// checks throughout the server. Explicitly nil-out the interface field when
	// the concrete pointer is nil.
	var iidx indexerIface
	if idx != nil {
		iidx = idx
	}
	return NewServerWithIndexer(q, iidx, w, nonCodeLangs)
}

// NewServerWithIndexer is like [NewServer] but accepts any value that satisfies
// [indexerIface]. This is useful in tests where a lightweight mock replaces the
// real [*indexer.Indexer].
func NewServerWithIndexer(q *query.Querier, idx indexerIface, w watcher.Watcher, nonCodeLangs []string) *Server {
	return &Server{querier: q, indexer: idx, watcher: w, nonCodeLanguages: nonCodeLangs}
}

// Serve runs the MCP stdio loop over os.Stdin and os.Stdout until ctx is
// cancelled. See ServeIO for control over the transport streams.
func (s *Server) Serve(ctx context.Context) error {
	return s.ServeIO(ctx, os.Stdin, os.Stdout)
}

// ServeIO runs the MCP stdio loop over the provided streams until ctx is
// cancelled. It performs an initial full index (if indexer is non-nil),
// launches a background watcher goroutine (if both watcher and indexer are
// non-nil), and then blocks on the MCP stdio protocol loop. All goroutines
// are joined before ServeIO returns.
func (s *Server) ServeIO(ctx context.Context, in io.Reader, out io.Writer) error {
	// 1. Initial full index (serialized with watcher via indexMu).
	if s.indexer != nil {
		slog.InfoContext(ctx, "mcp: running initial index")
		s.indexMu.Lock()
		err := s.indexer.Index(ctx)
		if err == nil {
			s.lastRefresh = time.Now()
		}
		s.indexMu.Unlock()
		if err != nil {
			return fmt.Errorf("initial index: %w", err)
		}
		slog.InfoContext(ctx, "mcp: initial index complete")
	}

	// 2. Background watcher goroutine.
	var wg sync.WaitGroup
	if s.watcher != nil && s.indexer != nil {
		wg.Go(func() {
			s.watchLoop(ctx)
		})
	}

	// 3. Build and run the MCP server with context propagation.
	srv := mcp.NewServer(&mcp.Implementation{Name: "codamigo", Version: "0.1.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search",
		Description: "Semantic search over the indexed codebase",
	}, s.HandleSearch)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_map",
		Description: "Returns a structural map of the codebase showing packages, files, and symbol names. Use this to orient before searching. No API calls needed. By default, excludes configured non-code files and shows line ranges, type summaries, and visibility markers.",
	}, s.HandleMap)

	transport := &mcp.IOTransport{
		Reader: io.NopCloser(in),
		Writer: nopCloserWriter{out},
	}
	err := srv.Run(ctx, transport)

	// Wait for the watcher goroutine to finish before returning.
	wg.Wait()
	return err
}

// watchLoop reads batched events from the watcher and re-indexes changed paths.
func (s *Server) watchLoop(ctx context.Context) {
	ch := s.watcher.Watch(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case events, ok := <-ch:
			if !ok {
				return
			}
			var needsReindex bool
			paths := make([]string, 0, len(events))
			for _, e := range events {
				if e.Op == watcher.Reindex {
					needsReindex = true
					continue
				}
				paths = append(paths, e.Path)
			}
			s.indexMu.Lock()
			if needsReindex {
				if err := s.indexer.Index(ctx); err != nil {
					slog.ErrorContext(ctx, "mcp: full re-index after overflow failed", slog.Any("error", err))
				}
			} else if len(paths) > 0 {
				if err := s.indexer.IndexFiles(ctx, paths); err != nil {
					slog.ErrorContext(ctx, "mcp: re-index failed", slog.Any("error", err))
				}
			}
			s.indexMu.Unlock()
		}
	}
}

// SearchInput defines the parameters for the search MCP tool.
// The SDK infers the JSON schema from this struct's json and jsonschema tags.
type SearchInput struct {
	Query        string   `json:"query" jsonschema:"Search text to embed and match against indexed chunks"`
	Limit        int      `json:"limit,omitempty" jsonschema:"Maximum number of results to return (default 10)"`
	Languages    []string `json:"languages,omitempty" jsonschema:"Filter results by programming language (e.g. [\"go\", \"python\"])"`
	Paths        []string `json:"paths,omitempty" jsonschema:"Glob patterns to restrict search scope (e.g. [\"cmd/**\"])"`
	MaxTokens    int      `json:"max_tokens,omitempty" jsonschema:"Token budget for results. 0 = no limit (default 0)"`
	Package      string   `json:"package,omitempty" jsonschema:"Filter results to a package (e.g. \"store\", \"embedder/openaicompat\")"`
	RefreshIndex bool     `json:"refresh_index,omitempty" jsonschema:"Force a full re-index before querying (default false)"`
	Name         string   `json:"name,omitempty" jsonschema:"Filter results to chunks matching this symbol name (e.g. \"Search\", \"NewChunker\")"`
	NodeKinds    []string `json:"node_kinds,omitempty" jsonschema:"Filter by AST node kind (e.g. [\"function_declaration\", \"type_declaration\"])"`
	MetadataOnly bool     `json:"metadata_only,omitempty" jsonschema:"If true, return only file paths, line numbers, and symbol names without source code content. Use for exploratory queries to save tokens."`
	Offset       int      `json:"offset,omitempty" jsonschema:"Number of results to skip for pagination (default 0)"`
}

// HandleSearch is the tool handler for the search MCP tool.
// Exported to allow direct testing without the MCP stdio transport.
func (s *Server) HandleSearch(ctx context.Context, req *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, any, error) {
	if s.querier == nil {
		return newErrorResult("no querier configured"), nil, nil
	}

	queryText := input.Query
	if queryText == "" {
		return newErrorResult("query parameter is required"), nil, nil
	}

	limit := input.Limit
	maxTokens := max(0, input.MaxTokens)
	pkg := input.Package
	if pkg != "" {
		cleaned := filepath.Clean(pkg)
		if cleaned == "." || strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
			return newErrorResult("package must not contain '..' segments"), nil, nil
		}
		pkg = cleaned
	}
	refreshIndex := input.RefreshIndex

	// Cap array parameters at maxArraySize to prevent oversized queries.
	languages := capStringSlice(input.Languages)
	paths := capStringSlice(input.Paths)
	for _, p := range paths {
		cleaned := filepath.Clean(p)
		if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
			return newErrorResult("paths must not contain '..' segments"), nil, nil
		}
		if strings.Contains(p, "**") && !strings.HasSuffix(p, "/**") {
			return newErrorResult(
				fmt.Sprintf("unsupported glob %q: ** is only supported as trailing /**", p)), nil, nil
		}
	}
	name := input.Name
	nodeKinds := capStringSlice(input.NodeKinds)
	metadataOnly := input.MetadataOnly
	offset := max(0, input.Offset)

	// Clamp limit to safe range.
	// Values <= 0 default to 10; values > 100 clamped to 100
	// (100 chunks × ~500 tokens each ≈ 50 K tokens, near most AI context limits).
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 100)

	// Optional forced re-index, subject to a 30 s cooldown so that rapid
	// back-to-back calls cannot hammer the indexer.
	if refreshIndex && s.indexer != nil {
		s.indexMu.Lock()
		if time.Since(s.lastRefresh) >= refreshCooldown {
			err := s.indexer.Index(ctx)
			if err != nil {
				s.indexMu.Unlock()
				slog.ErrorContext(ctx, "re-index failed", slog.Any("error", err))
				return newErrorResult("re-index failed; check server logs"), nil, nil
			}
			s.lastRefresh = time.Now()
		}
		s.indexMu.Unlock()
	}

	opts := query.SearchOptions{
		Limit:        limit,
		Offset:       offset,
		Languages:    languages,
		Paths:        paths,
		MaxTokens:    maxTokens,
		Package:      pkg,
		MetadataOnly: metadataOnly,
		NodeKinds:    nodeKinds,
	}
	if name != "" {
		opts.Names = []string{name}
	}

	sr, err := s.querier.SearchWithOptions(ctx, queryText, opts)
	if err != nil {
		slog.ErrorContext(ctx, "search failed", slog.Any("error", err))
		return newErrorResult("search failed; check server logs"), nil, nil
	}

	if metadataOnly {
		var b strings.Builder
		for _, r := range sr.Results {
			fmt.Fprintf(&b, "%s:%d  %-20s %s\n", r.FilePath, r.StartLine, r.Name, r.NodeKind)
		}
		if sr.Truncated {
			b.WriteString("(truncated to token budget)\n")
		}
		return newTextResult(b.String()), nil, nil
	}

	type searchResponse struct {
		Results   []query.Result `json:"results"`
		Truncated bool           `json:"truncated"`
	}
	resp := searchResponse{Results: sr.Results, Truncated: sr.Truncated}
	data, err := json.Marshal(resp)
	if err != nil {
		slog.ErrorContext(ctx, "serializing results", slog.Any("error", err))
		return newErrorResult("internal error; check server logs"), nil, nil
	}

	return newTextResult(string(data)), nil, nil
}

// MapInput defines the parameters for the get_map MCP tool.
// Pointer fields are used where defaults differ from Go zero values.
type MapInput struct {
	MaxTokens      *int  `json:"max_tokens,omitempty" jsonschema:"Maximum token budget for the response (default 2000)"`
	CodeOnly       *bool `json:"code_only,omitempty" jsonschema:"Exclude configured non-code languages from the map (default true)"`
	ShowSummary    *bool `json:"show_summary,omitempty" jsonschema:"Show per-file type summary in file headers (default true)"`
	ShowVisibility *bool `json:"show_visibility,omitempty" jsonschema:"Show export markers: + for public, - for internal (default true)"`
}

// HandleMap is the tool handler for the get_map MCP tool.
// Exported to allow direct testing without the MCP stdio transport.
func (s *Server) HandleMap(ctx context.Context, req *mcp.CallToolRequest, input MapInput) (*mcp.CallToolResult, any, error) {
	if s.querier == nil {
		return newErrorResult("no querier configured"), nil, nil
	}
	maxTokens := 2000
	if input.MaxTokens != nil {
		maxTokens = *input.MaxTokens
	}
	maxTokens = max(0, maxTokens)
	codeOnly := true
	if input.CodeOnly != nil {
		codeOnly = *input.CodeOnly
	}
	showSummary := true
	if input.ShowSummary != nil {
		showSummary = *input.ShowSummary
	}
	showVisibility := true
	if input.ShowVisibility != nil {
		showVisibility = *input.ShowVisibility
	}
	opts := query.MapOptions{
		MaxTokens:        maxTokens,
		CodeOnly:         codeOnly,
		NonCodeLanguages: s.nonCodeLanguages,
		ShowSummary:      showSummary,
		ShowVisibility:   showVisibility,
	}
	result, err := s.querier.Map(ctx, opts)
	if err != nil {
		slog.ErrorContext(ctx, "map generation failed", slog.Any("error", err))
		return newErrorResult("map generation failed; check server logs"), nil, nil
	}
	if result == "" {
		return newTextResult("No symbols indexed yet. Run indexing first."), nil, nil
	}
	return newTextResult(result), nil, nil
}

// maxArraySize is the maximum number of elements accepted from any array
// parameter. Inputs beyond this limit are silently truncated to prevent
// oversized queries from reaching the store layer.
const maxArraySize = 50

// capStringSlice truncates s to at most maxArraySize elements.
func capStringSlice(s []string) []string {
	if len(s) > maxArraySize {
		return s[:maxArraySize]
	}
	return s
}

// newTextResult creates a CallToolResult containing a single text content block.
func newTextResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// newErrorResult creates a CallToolResult with IsError set to true and an error
// message in the content.
func newErrorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

// nopCloserWriter wraps an io.Writer to satisfy io.WriteCloser with a no-op Close.
type nopCloserWriter struct {
	io.Writer
}

func (nopCloserWriter) Close() error { return nil }
