// Package query implements semantic search from a caller's perspective.
//
// [Querier] wraps an embedder and a store: its [Querier.Search] and
// [Querier.SearchWithOptions] methods embed the query string and run hybrid
// KNN + BM25 search over the store. [Result] is a separate type from
// store.SearchResult so query controls its own public surface independently
// of storage internals.
//
// [Querier.Map] generates a token-budget-aware repo map by listing all
// named symbols from the store, grouping them by package, and rendering
// a compact directory of types and functions.
package query

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/go-embedder"
)

// Result is a matching code chunk returned by [Querier.Search] or
// [Querier.SearchWithOptions], with a relevance Score.
type Result struct {
	FilePath  string
	Language  string
	Content   string
	NodeKind  string
	Name      string
	Parent    string
	StartLine int
	EndLine   int
	Score     float32
}

// SearchResults wraps a slice of results with a truncation flag indicating
// whether the token budget caused some results to be dropped.
type SearchResults struct {
	Results   []Result // matching chunks in relevance order
	Truncated bool     // true when MaxTokens budget caused results to be dropped
}

// SearchOptions configures a [Querier.SearchWithOptions] call.
type SearchOptions struct {
	Limit        int      // maximum number of results to return
	Offset       int      // number of results to skip (pagination)
	Languages    []string // filter by language name; nil means all
	Paths        []string // glob patterns for file path filtering
	MaxTokens    int      // token budget; 0 means no limit
	Package      string   // filter to a package (e.g. "store"); resolved to a Paths glob
	Names        []string // filter by symbol name; nil means all
	NodeKinds    []string // filter by node kind; nil means all
	MetadataOnly bool     // when true, return metadata only (no content)
}

// MapOptions configures a [Querier.Map] call.
type MapOptions struct {
	MaxTokens        int      // token budget; 0 means no limit
	CodeOnly         bool     // exclude languages listed in NonCodeLanguages
	NonCodeLanguages []string // language names excluded when CodeOnly is true
	ShowSummary      bool     // add per-file kind count summary in file headers
	ShowVisibility   bool     // prefix symbols with + (exported) or - (internal)
}

// mapCache implements a lock-free read / mutex-protected rebuild cache.
// Reads atomically load generation and cached pointer with no locking.
// Rebuilds acquire mu to serialize and double-check the generation, preventing
// stampede when multiple goroutines see a stale cache simultaneously.
type mapCache struct {
	generation atomic.Uint64
	cached     atomic.Pointer[cachedMap]
	mu         sync.Mutex
}

// cachedMap is the value stored by mapCache. gen must equal the generation
// counter at the time of caching; a mismatch means the cache is stale.
type cachedMap struct {
	gen      uint64
	packages []packageGroup
}

// Querier runs semantic search over an indexed store.
type Querier struct {
	embedder embedder.Embedder
	store    store.Store
	cache    *lru.Cache[string, []float32]
	mc       mapCache
}

// New constructs a Querier backed by the given embedder and store.
func New(e embedder.Embedder, s store.Store) *Querier {
	cache, err := lru.New[string, []float32](64)
	if err != nil {
		panic(fmt.Sprintf("query: creating LRU cache: %v", err))
	}
	return &Querier{
		embedder: e,
		store:    s,
		cache:    cache,
	}
}

// Search embeds text and returns the top limit most similar records from the store.
func (q *Querier) Search(ctx context.Context, text string, limit int) (SearchResults, error) {
	return q.SearchWithOptions(ctx, text, SearchOptions{Limit: limit})
}

// SearchWithOptions is like Search but also accepts offset, language, path,
// package, and token-budget filters via [SearchOptions].
func (q *Querier) SearchWithOptions(ctx context.Context, text string, opts SearchOptions) (SearchResults, error) {
	var emb []float32
	if cached, ok := q.cache.Get(text); ok {
		emb = cached
	} else {
		var err error
		emb, err = q.embedder.Embed(ctx, text)
		if err != nil {
			return SearchResults{}, fmt.Errorf("embedding query: %w", err)
		}
		q.cache.Add(text, emb)
	}

	paths := opts.Paths
	if opts.Package != "" {
		pkgGlob := opts.Package + "/**"
		paths = append(append([]string{}, paths...), pkgGlob)
	}

	results, err := q.store.Search(ctx, store.SearchQuery{
		Embedding:    emb,
		Text:         text,
		Limit:        opts.Limit,
		Offset:       opts.Offset,
		Languages:    opts.Languages,
		Paths:        paths,
		Names:        opts.Names,
		NodeKinds:    opts.NodeKinds,
		MetadataOnly: opts.MetadataOnly,
	})
	if err != nil {
		return SearchResults{}, fmt.Errorf("searching store: %w", err)
	}

	out := make([]Result, len(results))
	for i, r := range results {
		out[i] = Result{
			FilePath:  r.FilePath,
			Language:  r.Language,
			Content:   r.Content,
			NodeKind:  r.NodeKind,
			Name:      r.Name,
			Parent:    r.Parent,
			StartLine: r.StartLine,
			EndLine:   r.EndLine,
			Score:     r.Score,
		}
	}

	if opts.MaxTokens > 0 {
		return truncateResults(out, opts.MaxTokens), nil
	}
	return SearchResults{Results: out, Truncated: false}, nil
}

// truncateResults keeps results that fit within the token budget (1 token ~= 4 chars).
// The first result is always included regardless of budget.
func truncateResults(results []Result, maxTokens int) SearchResults {
	if len(results) == 0 {
		return SearchResults{Results: results, Truncated: false}
	}

	kept := make([]Result, 0, len(results))
	used := 0
	for i, r := range results {
		tokens := len(r.Content) / 4
		if i > 0 && used+tokens > maxTokens {
			return SearchResults{Results: kept, Truncated: true}
		}
		kept = append(kept, r)
		used += tokens
	}
	return SearchResults{Results: kept, Truncated: false}
}

// PackageFromPath derives a Go-style package path from a file path relative to projectRoot.
// Returns "." for files directly in the project root.
func PackageFromPath(projectRoot, filePath string) string {
	rel, err := filepath.Rel(projectRoot, filePath)
	if err != nil {
		rel = filePath
	}
	dir := filepath.Dir(rel)
	if dir == "." {
		return "."
	}
	return filepath.ToSlash(dir)
}

// InvalidateMapCache bumps the generation counter so the next Map call
// rebuilds the cached output from the store.
func (q *Querier) InvalidateMapCache() {
	q.mc.generation.Add(1)
}

// Map generates a token-budget-aware repository map by listing all named
// symbols from the store, grouping them by package, and rendering a compact
// directory. Packages with more symbols appear first. Results are cached by
// generation counter; call [Querier.InvalidateMapCache] after indexing new
// content to force a rebuild. The cache stores unfiltered data; all
// [MapOptions] filtering is applied at render time.
func (q *Querier) Map(ctx context.Context, opts MapOptions) (string, error) {
	gen := q.mc.generation.Load()
	if c := q.mc.cached.Load(); c != nil && c.gen == gen {
		return renderMap(c.packages, opts), nil
	}

	q.mc.mu.Lock()
	defer q.mc.mu.Unlock()

	// Double-check after acquiring lock.
	gen = q.mc.generation.Load()
	if c := q.mc.cached.Load(); c != nil && c.gen == gen {
		return renderMap(c.packages, opts), nil
	}

	symbols, err := q.store.ListSymbols(ctx)
	if err != nil {
		return "", fmt.Errorf("listing symbols: %w", err)
	}
	if len(symbols) == 0 {
		return "", nil
	}

	projectRoot := deriveProjectRoot(symbols)
	grouped := groupSymbolsByPackage(symbols, projectRoot)
	ranked := rankPackages(grouped)

	q.mc.cached.Store(&cachedMap{gen: gen, packages: ranked})

	return renderMap(ranked, opts), nil
}

// packageGroup aggregates all files and symbols belonging to one package path
// for repo-map rendering. fileIndex is a scratch map used only during
// construction (groupSymbolsByPackage) and is nilled out before caching.
type packageGroup struct {
	name      string
	files     []fileGroup
	fileIndex map[string]int // relPath -> index in files; nil after construction
	totalSyms int
}

// fileGroup holds all symbols extracted from one source file for repo-map rendering.
type fileGroup struct {
	relPath string
	symbols []store.Symbol
}

// deriveProjectRoot finds the longest common directory prefix across all symbol file paths.
func deriveProjectRoot(symbols []store.Symbol) string {
	if len(symbols) == 0 {
		return ""
	}
	prefix := filepath.Dir(symbols[0].FilePath)
	for _, sym := range symbols[1:] {
		dir := filepath.Dir(sym.FilePath)
		for dir != prefix && !strings.HasPrefix(dir, prefix+string(filepath.Separator)) {
			parent := filepath.Dir(prefix)
			if parent == prefix {
				return prefix
			}
			prefix = parent
		}
	}
	return prefix
}

// groupSymbolsByPackage organizes symbols into package groups, each containing file groups.
func groupSymbolsByPackage(symbols []store.Symbol, projectRoot string) map[string]*packageGroup {
	groups := make(map[string]*packageGroup)
	for _, sym := range symbols {
		pkg := PackageFromPath(projectRoot, sym.FilePath)
		g, ok := groups[pkg]
		if !ok {
			g = &packageGroup{name: pkg, fileIndex: make(map[string]int)}
			groups[pkg] = g
		}

		rel, err := filepath.Rel(projectRoot, sym.FilePath)
		if err != nil {
			rel = sym.FilePath
		}
		rel = filepath.ToSlash(rel)

		if idx, exists := g.fileIndex[rel]; exists {
			g.files[idx].symbols = append(g.files[idx].symbols, sym)
		} else {
			g.fileIndex[rel] = len(g.files)
			g.files = append(g.files, fileGroup{relPath: rel, symbols: []store.Symbol{sym}})
		}
		g.totalSyms++
	}
	return groups
}

// rankPackages sorts packages by descending symbol count and files within each
// package by descending symbol count.
func rankPackages(groups map[string]*packageGroup) []packageGroup {
	ranked := make([]packageGroup, 0, len(groups))
	for _, g := range groups {
		slices.SortFunc(g.files, func(a, b fileGroup) int {
			return cmp.Compare(len(b.symbols), len(a.symbols))
		})
		g.fileIndex = nil
		ranked = append(ranked, *g)
	}
	slices.SortFunc(ranked, func(a, b packageGroup) int {
		return cmp.Compare(b.totalSyms, a.totalSyms)
	})
	return ranked
}

var nodeKindLabels = map[string]string{
	"function_declaration":  "func",
	"function_definition":   "func",
	"method_declaration":    "func",
	"method_definition":     "func",
	"type_declaration":      "type",
	"type_spec":             "type",
	"class_declaration":     "class",
	"class_definition":      "class",
	"interface_declaration": "interface",
	"interface_type":        "interface",
	"struct_type":           "struct",
	"export_statement":      "export",
}

func formatNodeKind(nodeKind string) string {
	if label, ok := nodeKindLabels[nodeKind]; ok {
		return label
	}
	return nodeKind
}

// filterCodeFiles returns a new packageGroup with non-code files removed.
// A file is non-code when its language (from the first symbol) is in nonCode.
// The input packageGroup is never mutated.
func filterCodeFiles(pkg packageGroup, nonCode map[string]struct{}) packageGroup {
	files := make([]fileGroup, 0, len(pkg.files))
	totalSyms := 0
	for _, f := range pkg.files {
		if len(f.symbols) > 0 {
			if _, skip := nonCode[f.symbols[0].Language]; skip {
				continue
			}
		}
		files = append(files, f)
		totalSyms += len(f.symbols)
	}
	return packageGroup{name: pkg.name, files: files, totalSyms: totalSyms}
}

// isExported returns a visibility marker for the given symbol based on
// language conventions. Returns "+ " for exported/public symbols,
// "- " for internal symbols, or "" when the language has no visibility
// convention (e.g. C, Bash). An empty name always returns "".
func isExported(name, language, nodeKind string) string {
	if name == "" {
		return ""
	}
	switch language {
	case "go":
		r, size := utf8.DecodeRuneInString(name)
		if r == utf8.RuneError && size <= 1 {
			return ""
		}
		if unicode.IsUpper(r) {
			return "+ "
		}
		return "- "
	case "python", "ruby":
		if strings.HasPrefix(name, "_") {
			return "- "
		}
		return "+ "
	case "javascript", "typescript", "tsx":
		if nodeKind == "export_statement" {
			return "+ "
		}
		return "- "
	default:
		return ""
	}
}

// kindSummary returns a parenthesized summary of symbol kinds in a file,
// e.g. "(3 func, 2 type, 1 interface)". Kinds are sorted alphabetically
// for deterministic output. Returns "" when the file has fewer than
// minSummarySymbols symbols.
func kindSummary(symbols []store.Symbol) string {
	const minSummarySymbols = 5
	if len(symbols) < minSummarySymbols {
		return ""
	}
	counts := make(map[string]int)
	for _, sym := range symbols {
		label := formatNodeKind(sym.NodeKind)
		counts[label]++
	}
	keys := slices.Sorted(maps.Keys(counts))

	var b strings.Builder
	b.WriteString(" (")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d %s", counts[k], k)
	}
	b.WriteString(")")
	return b.String()
}

// formatLineRange returns a ":start-end" suffix for a symbol's line span,
// or ":line" when start equals end.
func formatLineRange(startLine, endLine int) string {
	if startLine == endLine {
		return fmt.Sprintf(":%d", startLine)
	}
	return fmt.Sprintf(":%d-%d", startLine, endLine)
}

// renderMap formats ranked packages into a text repo map, respecting the token
// budget and applying all [MapOptions] rendering enhancements. It reads but
// never mutates the packages slice (safe for concurrent use with cached data).
func renderMap(packages []packageGroup, opts MapOptions) string {
	// Apply code-only filter (builds new slices, never mutates input).
	if opts.CodeOnly {
		nonCode := make(map[string]struct{}, len(opts.NonCodeLanguages))
		for _, lang := range opts.NonCodeLanguages {
			nonCode[lang] = struct{}{}
		}
		filtered := make([]packageGroup, 0, len(packages))
		for _, pkg := range packages {
			fp := filterCodeFiles(pkg, nonCode)
			if fp.totalSyms > 0 {
				filtered = append(filtered, fp)
			}
		}
		packages = filtered
	}

	var b strings.Builder
	budget := opts.MaxTokens * 4

	totalFiles := 0
	for _, pkg := range packages {
		totalFiles += len(pkg.files)
	}

	filesWritten := 0
	pkgsWritten := 0
	truncated := false

	for _, pkg := range packages {
		header := fmt.Sprintf("# package: %s (%d files, %d symbols)\n\n", pkg.name, len(pkg.files), pkg.totalSyms)

		if budget > 0 && b.Len()+len(header) > budget && filesWritten > 0 {
			truncated = true
			break
		}
		b.WriteString(header)

		pkgFilesWritten := 0
		for _, f := range pkg.files {
			var fb strings.Builder
			// File header with optional kind summary.
			fb.WriteString(f.relPath)
			if opts.ShowSummary {
				fb.WriteString(kindSummary(f.symbols))
			}
			fb.WriteString("\n")

			// Determine the language for visibility from the first symbol.
			fileLang := ""
			if len(f.symbols) > 0 {
				fileLang = f.symbols[0].Language
			}

			// Build parent→children index for nesting.
			children := make(map[string][]store.Symbol, len(f.symbols)/2)
			topLevel := make([]store.Symbol, 0, len(f.symbols))
			for _, sym := range f.symbols {
				if sym.Parent == "" {
					topLevel = append(topLevel, sym)
				} else {
					children[sym.Parent] = append(children[sym.Parent], sym)
				}
			}

			// Render top-level symbols with their children grouped underneath.
			rendered := make(map[string]struct{}, len(f.symbols))
			for _, sym := range topLevel {
				vis := ""
				if opts.ShowVisibility {
					vis = isExported(sym.Name, fileLang, sym.NodeKind)
				}
				lineRange := formatLineRange(sym.StartLine, sym.EndLine)
				fb.WriteString(fmt.Sprintf("  %s%s %s%s\n", vis, formatNodeKind(sym.NodeKind), sym.Name, lineRange))

				// Render children of this top-level symbol.
				for _, child := range children[sym.Name] {
					childVis := ""
					if opts.ShowVisibility {
						childVis = isExported(child.Name, fileLang, child.NodeKind)
					}
					childLineRange := formatLineRange(child.StartLine, child.EndLine)
					fb.WriteString(fmt.Sprintf("    %s%s %s%s\n", childVis, formatNodeKind(child.NodeKind), child.Name, childLineRange))
					rendered[child.Parent+"\x00"+child.Name+"\x00"+strconv.Itoa(child.StartLine)] = struct{}{}
				}
			}

			// Render orphan children (Parent set but no matching top-level symbol).
			for _, sym := range f.symbols {
				if sym.Parent == "" {
					continue
				}
				key := sym.Parent + "\x00" + sym.Name + "\x00" + strconv.Itoa(sym.StartLine)
				if _, ok := rendered[key]; ok {
					continue
				}
				vis := ""
				if opts.ShowVisibility {
					vis = isExported(sym.Name, fileLang, sym.NodeKind)
				}
				lineRange := formatLineRange(sym.StartLine, sym.EndLine)
				fb.WriteString(fmt.Sprintf("  %s%s %s%s\n", vis, formatNodeKind(sym.NodeKind), sym.Name, lineRange))
			}

			fb.WriteString("\n")

			chunk := fb.String()
			if budget > 0 && b.Len()+len(chunk) > budget && filesWritten > 0 {
				truncated = true
				break
			}
			b.WriteString(chunk)
			filesWritten++
			pkgFilesWritten++
		}
		if pkgFilesWritten > 0 {
			pkgsWritten++
		}
		if truncated {
			break
		}
	}

	if truncated {
		remainingFiles := totalFiles - filesWritten
		remainingPkgs := len(packages) - pkgsWritten
		b.WriteString(fmt.Sprintf("# ... %d more files in %d packages (truncated to ~%d tokens)\n", remainingFiles, remainingPkgs, opts.MaxTokens))
	}

	return b.String()
}
