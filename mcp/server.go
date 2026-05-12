// Package mcp exposes codamigo's search capability as an MCP (Model Context
// Protocol) stdio server.
//
// [Server] is constructed with a [*query.Querier] and optional
// [*indexer.Indexer] and [watcher.Watcher]. [Server.Serve] runs the MCP
// stdio loop until the context is cancelled. The server exposes a single
// "search" tool with query, limit, and refresh_index parameters. When no
// Indexer is provided, refresh_index=true is silently ignored.
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

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"github.com/ieshan/codamigo/indexer"
	"github.com/ieshan/codamigo/query"
	"github.com/ieshan/codamigo/watcher"
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

	// 3. Build and run the MCP stdio server with context propagation.
	srv := mcpserver.NewMCPServer("codamigo", "0.1.0")
	srv.AddTool(s.buildSearchTool(), s.HandleSearch)
	srv.AddTool(s.buildMapTool(), s.HandleMap)

	stdio := mcpserver.NewStdioServer(srv)
	err := stdio.Listen(ctx, in, out)

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
			paths := make([]string, 0, len(events))
			for _, e := range events {
				paths = append(paths, e.Path)
			}
			s.indexMu.Lock()
			if err := s.indexer.IndexFiles(ctx, paths); err != nil {
				slog.ErrorContext(ctx, "mcp: re-index failed", slog.Any("error", err))
			}
			s.indexMu.Unlock()
		}
	}
}

// buildSearchTool returns the MCP tool definition for the search command.
func (s *Server) buildSearchTool() mcpgo.Tool {
	return mcpgo.NewTool("search",
		mcpgo.WithDescription("Semantic search over the indexed codebase"),
		mcpgo.WithString("query",
			mcpgo.Required(),
			mcpgo.Description("Search text to embed and match against indexed chunks"),
		),
		mcpgo.WithInteger("limit",
			mcpgo.Description("Maximum number of results to return (default 10)"),
			mcpgo.DefaultNumber(10),
		),
		mcpgo.WithArray("languages",
			mcpgo.Description("Filter results by programming language (e.g. [\"go\", \"python\"])"),
		),
		mcpgo.WithArray("paths",
			mcpgo.Description("Glob patterns to restrict search scope (e.g. [\"cmd/**\"])"),
		),
		mcpgo.WithInteger("max_tokens",
			mcpgo.Description("Token budget for results. 0 = no limit (default 0)"),
			mcpgo.DefaultNumber(0),
		),
		mcpgo.WithString("package",
			mcpgo.Description("Filter results to a package (e.g. \"store\", \"embedder/openaicompat\")"),
		),
		mcpgo.WithBoolean("refresh_index",
			mcpgo.Description("Force a full re-index before querying (default false)"),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithString("name",
			mcpgo.Description("Filter results to chunks matching this symbol name (e.g. \"Search\", \"NewChunker\")"),
		),
		mcpgo.WithArray("node_kinds",
			mcpgo.Description("Filter by AST node kind (e.g. [\"function_declaration\", \"type_declaration\"])"),
		),
		mcpgo.WithBoolean("metadata_only",
			mcpgo.Description("If true, return only file paths, line numbers, and symbol names without source code content. Use for exploratory queries to save tokens."),
			mcpgo.DefaultBool(false),
		),
		mcpgo.WithInteger("offset",
			mcpgo.Description("Number of results to skip for pagination (default 0)"),
			mcpgo.DefaultNumber(0),
		),
	)
}

// HandleSearch is the ToolHandlerFunc for the search tool.
// Exported to allow direct testing without the MCP stdio transport.
func (s *Server) HandleSearch(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.querier == nil {
		return mcpgo.NewToolResultError("no querier configured"), nil
	}

	queryText := mcpgo.ParseString(req, "query", "")
	if queryText == "" {
		return mcpgo.NewToolResultError("query parameter is required"), nil
	}

	limit := mcpgo.ParseInt(req, "limit", 10)
	maxTokens := mcpgo.ParseInt(req, "max_tokens", 0)
	pkg := mcpgo.ParseString(req, "package", "")
	if pkg != "" {
		cleaned := filepath.Clean(pkg)
		if cleaned == "." || strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
			return mcpgo.NewToolResultError("package must not contain '..' segments"), nil
		}
		pkg = cleaned
	}
	refreshIndex := mcpgo.ParseBoolean(req, "refresh_index", false)

	// Parse optional array parameters.
	languages := parseStringArray(req, "languages")
	paths := parseStringArray(req, "paths")
	for _, p := range paths {
		cleaned := filepath.Clean(p)
		if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
			return mcpgo.NewToolResultError("paths must not contain '..' segments"), nil
		}
		if strings.Contains(p, "**") && !strings.HasSuffix(p, "/**") {
			return mcpgo.NewToolResultError(
				fmt.Sprintf("unsupported glob %q: ** is only supported as trailing /**", p)), nil
		}
	}
	name := mcpgo.ParseString(req, "name", "")
	nodeKinds := parseStringArray(req, "node_kinds")
	metadataOnly := mcpgo.ParseBoolean(req, "metadata_only", false)
	offset := mcpgo.ParseInt(req, "offset", 0)

	// Clamp parameters to safe ranges.
	// limit: values <= 0 default to 10; values > 100 clamped to 100
	// (100 chunks × ~500 tokens each ≈ 50 K tokens, near most AI context limits).
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	// max_tokens: negative values treated as 0 (unlimited).
	if maxTokens < 0 {
		maxTokens = 0
	}
	// offset: negative values treated as 0.
	if offset < 0 {
		offset = 0
	}

	// Optional forced re-index, subject to a 30 s cooldown so that rapid
	// back-to-back calls cannot hammer the indexer.
	if refreshIndex && s.indexer != nil {
		s.indexMu.Lock()
		if time.Since(s.lastRefresh) >= refreshCooldown {
			err := s.indexer.Index(ctx)
			if err != nil {
				s.indexMu.Unlock()
				slog.ErrorContext(ctx, "re-index failed", slog.Any("error", err))
				return mcpgo.NewToolResultError("re-index failed; check server logs"), nil
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
		return mcpgo.NewToolResultError("search failed; check server logs"), nil
	}

	if metadataOnly {
		var b strings.Builder
		for _, r := range sr.Results {
			fmt.Fprintf(&b, "%s:%d  %-20s %s\n", r.FilePath, r.StartLine, r.Name, r.NodeKind)
		}
		if sr.Truncated {
			b.WriteString("(truncated to token budget)\n")
		}
		return mcpgo.NewToolResultText(b.String()), nil
	}

	type searchResponse struct {
		Results   []query.Result `json:"results"`
		Truncated bool           `json:"truncated"`
	}
	resp := searchResponse{Results: sr.Results, Truncated: sr.Truncated}
	data, err := json.Marshal(resp)
	if err != nil {
		slog.ErrorContext(ctx, "serializing results", slog.Any("error", err))
		return mcpgo.NewToolResultError("internal error; check server logs"), nil
	}

	return mcpgo.NewToolResultText(string(data)), nil
}

// buildMapTool returns the MCP tool definition for the get_map command.
func (s *Server) buildMapTool() mcpgo.Tool {
	return mcpgo.NewTool("get_map",
		mcpgo.WithDescription("Returns a structural map of the codebase showing packages, files, and symbol names. Use this to orient before searching. No API calls needed. By default, excludes configured non-code files and shows line ranges, type summaries, and visibility markers."),
		mcpgo.WithInteger("max_tokens",
			mcpgo.Description("Maximum token budget for the response (default 2000)"),
			mcpgo.DefaultNumber(2000),
		),
		mcpgo.WithBoolean("code_only",
			mcpgo.Description("Exclude configured non-code languages from the map (default true)"),
			mcpgo.DefaultBool(true),
		),
		mcpgo.WithBoolean("show_summary",
			mcpgo.Description("Show per-file type summary in file headers (default true)"),
			mcpgo.DefaultBool(true),
		),
		mcpgo.WithBoolean("show_visibility",
			mcpgo.Description("Show export markers: + for public, - for internal (default true)"),
			mcpgo.DefaultBool(true),
		),
	)
}

// HandleMap is the ToolHandlerFunc for the get_map tool.
// Exported to allow direct testing without the MCP stdio transport.
func (s *Server) HandleMap(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	if s.querier == nil {
		return mcpgo.NewToolResultError("no querier configured"), nil
	}
	maxTokens := mcpgo.ParseInt(req, "max_tokens", 2000)
	if maxTokens < 0 {
		maxTokens = 0
	}
	opts := query.MapOptions{
		MaxTokens:        maxTokens,
		CodeOnly:         mcpgo.ParseBoolean(req, "code_only", true),
		NonCodeLanguages: s.nonCodeLanguages,
		ShowSummary:      mcpgo.ParseBoolean(req, "show_summary", true),
		ShowVisibility:   mcpgo.ParseBoolean(req, "show_visibility", true),
	}
	result, err := s.querier.Map(ctx, opts)
	if err != nil {
		slog.ErrorContext(ctx, "map generation failed", slog.Any("error", err))
		return mcpgo.NewToolResultError("map generation failed; check server logs"), nil
	}
	if result == "" {
		return mcpgo.NewToolResultText("No symbols indexed yet. Run indexing first."), nil
	}
	return mcpgo.NewToolResultText(result), nil
}

// maxArraySize is the maximum number of elements accepted from any array
// parameter. Inputs beyond this limit are silently truncated to prevent
// oversized queries from reaching the store layer.
const maxArraySize = 50

// parseStringArray extracts a []string from a tool request argument.
// Returns nil when the key is absent or the value is not a string slice.
// The returned slice is capped at maxArraySize elements.
func parseStringArray(req mcpgo.CallToolRequest, key string) []string {
	raw := mcpgo.ParseArgument(req, key, nil)
	if raw == nil {
		return nil
	}
	slice, ok := raw.([]any)
	if !ok {
		return nil
	}
	if len(slice) > maxArraySize {
		slice = slice[:maxArraySize]
	}
	out := make([]string, 0, len(slice))
	for _, v := range slice {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
