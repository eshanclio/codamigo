package query

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ieshan/codamigo/store"
)

// ErrGraphNotBuilt is returned by the graph queries when the index holds no
// edges at all, which means indexing ran with the graph disabled. Edges cannot
// be derived from stored chunks, so the source must be re-indexed with the graph
// enabled. It is distinct from a symbol legitimately having no callers.
var ErrGraphNotBuilt = errors.New("code graph not built; re-run indexing")

// defaultImpactDepth is used when Impact is called with a non-positive depth.
const defaultImpactDepth = 2

// maxImpactDepth bounds traversal so a pathological graph cannot spin.
const maxImpactDepth = 10

// edgeKindCall and edgeKindReference mirror chunker.EdgeCall and
// chunker.EdgeReference. They are duplicated as strings so query stays
// independent of the chunker, matching how store does it.
const (
	edgeKindCall      = "call"
	edgeKindReference = "reference"
)

// GraphRef is one endpoint of a relationship, as reported to callers.
//
// A ref is Resolved when the target was found among the indexed definitions.
// Unresolved refs are still returned: a call into a third-party package is real
// information even though the callee is not in this project.
type GraphRef struct {
	Name      string // symbol name, or the identifier as written when unresolved
	Qualifier string // receiver or package part of the reference, when it had one
	EdgeKind  string // relationship that produced this ref: call, inherit, reference
	Resolved  bool   // true when Name matched an indexed definition
	FilePath  string // definition location; empty when unresolved
	NodeKind  string // definition node kind; empty when unresolved
	Parent    string // containing symbol of the definition; empty when unresolved or top-level
	StartLine int    // 1-based start line of the definition; 0 when unresolved
	EndLine   int    // 1-based end line of the definition; 0 when unresolved
	Line      int    // 1-based line of the reference itself
	RefFile   string // file containing the reference
	Depth     int    // traversal distance from the queried symbol; 1 for direct
}

// symbolCache caches the name and ID indexes built from store.ListSymbols,
// following the same lock-free-read / locked-rebuild pattern as mapCache and
// gated by the same generation counter.
type symbolCache struct {
	cached atomic.Pointer[cachedSymbols]
	mu     sync.Mutex
}

// cachedSymbols indexes the store's symbols for resolution. gen must equal the
// Querier's generation counter; a mismatch means the cache is stale.
type cachedSymbols struct {
	gen    uint64
	byName map[string][]store.Symbol
	byID   map[string]store.Symbol
}

// symbols returns the cached symbol indexes, rebuilding them when the
// generation counter has moved. It reuses the data ListSymbols already provides
// for the repo map, so graph queries add no extra store round-trips.
func (q *Querier) symbols(ctx context.Context) (*cachedSymbols, error) {
	gen := q.generation.Load()
	if c := q.sc.cached.Load(); c != nil && c.gen == gen {
		return c, nil
	}

	q.sc.mu.Lock()
	defer q.sc.mu.Unlock()

	// Double-check after acquiring the lock.
	gen = q.generation.Load()
	if c := q.sc.cached.Load(); c != nil && c.gen == gen {
		return c, nil
	}

	list, err := q.store.ListSymbols(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing symbols: %w", err)
	}

	c := &cachedSymbols{
		gen:    gen,
		byName: make(map[string][]store.Symbol, len(list)),
		byID:   make(map[string]store.Symbol, len(list)),
	}
	for _, sym := range list {
		c.byName[sym.Name] = append(c.byName[sym.Name], sym)
		c.byID[sym.ID] = sym
	}
	q.sc.cached.Store(c)
	return c, nil
}

// Callees returns what the given symbol references: the functions it calls, the
// types it names, and the supertypes it declares. Unresolved targets are
// included, so calls into dependencies are still reported.
func (q *Querier) Callees(ctx context.Context, symbol string) ([]GraphRef, error) {
	syms, err := q.symbols(ctx)
	if err != nil {
		return nil, err
	}

	defs := syms.byName[symbol]
	if len(defs) == 0 {
		return nil, q.emptyReason(ctx)
	}

	srcIDs := make([]string, 0, len(defs))
	for _, d := range defs {
		srcIDs = append(srcIDs, d.ID)
	}

	edges, err := q.store.EdgesBySource(ctx, srcIDs)
	if err != nil {
		return nil, fmt.Errorf("loading edges: %w", err)
	}
	if len(edges) == 0 {
		return nil, q.emptyReason(ctx)
	}

	imports, err := q.importIndex(ctx, edgeFiles(edges))
	if err != nil {
		return nil, err
	}

	refs := make([]GraphRef, 0, len(edges))
	for _, e := range edges {
		ref := GraphRef{
			Name:      e.DstName,
			Qualifier: e.DstQualifier,
			EdgeKind:  e.Kind,
			Line:      e.Line,
			RefFile:   e.FilePath,
			Depth:     1,
		}
		if best, ok := q.resolve(syms, imports, e.DstName, e.DstQualifier, e.FilePath); ok {
			ref.Resolved = true
			ref.FilePath = best.FilePath
			ref.NodeKind = best.NodeKind
			ref.Parent = best.Parent
			ref.StartLine = best.StartLine
			ref.EndLine = best.EndLine
		} else if e.Kind == edgeKindReference {
			// An unresolved type reference is almost always a builtin or a
			// stdlib type (error, string, context.Context). Reporting those as
			// dependencies is noise; unresolved calls are kept because a call
			// into a dependency is real information.
			continue
		}
		refs = append(refs, ref)
	}
	return dedupeRefs(refs), nil
}

// Callers returns the definitions that reference the given symbol.
//
// Matching is by name: when two definitions share a name, every reference to
// either is reported. Narrowing further would need type inference, so the
// over-approximation is deliberate — for "what breaks if I change this?" a
// false positive costs a glance, a false negative costs a bug.
func (q *Querier) Callers(ctx context.Context, symbol string) ([]GraphRef, error) {
	syms, err := q.symbols(ctx)
	if err != nil {
		return nil, err
	}
	refs, err := q.callersOf(ctx, syms, symbol)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, q.emptyReason(ctx)
	}
	// One entry per calling definition: a function that calls the symbol three
	// times is one caller, not three.
	return dedupeByDefinition(refs), nil
}

// Impact returns the definitions transitively affected by changing the given
// symbol: its callers, their callers, and so on up to depth. Depth defaults to
// [defaultImpactDepth] and is capped at [maxImpactDepth]. Cycles terminate
// because each symbol is visited once.
func (q *Querier) Impact(ctx context.Context, symbol string, depth int) ([]GraphRef, error) {
	if depth <= 0 {
		depth = defaultImpactDepth
	}
	depth = min(depth, maxImpactDepth)

	syms, err := q.symbols(ctx)
	if err != nil {
		return nil, err
	}

	var (
		out      []GraphRef
		visited  = map[string]struct{}{symbol: {}}
		frontier = []string{symbol}
	)

	for d := 1; d <= depth && len(frontier) > 0; d++ {
		var next []string
		for _, name := range frontier {
			refs, err := q.callersOf(ctx, syms, name)
			if err != nil {
				return nil, err
			}
			for _, ref := range dedupeByDefinition(refs) {
				if _, seen := visited[ref.Name]; seen {
					continue
				}
				visited[ref.Name] = struct{}{}
				ref.Depth = d
				out = append(out, ref)
				// Every caller is itself a named definition, so it is always a
				// candidate for the next level.
				next = append(next, ref.Name)
			}
		}
		frontier = next
	}

	if len(out) == 0 {
		return nil, q.emptyReason(ctx)
	}
	return out, nil
}

// callersOf finds the definitions holding a reference to symbol, matched on the
// edge's target name. The referencing definition is then resolved to a location.
func (q *Querier) callersOf(ctx context.Context, syms *cachedSymbols, symbol string) ([]GraphRef, error) {
	edges, err := q.store.EdgesByTargetName(ctx, symbol)
	if err != nil {
		return nil, fmt.Errorf("loading edges: %w", err)
	}
	if len(edges) == 0 {
		return nil, nil
	}

	imports, err := q.importIndex(ctx, edgeFiles(edges))
	if err != nil {
		return nil, err
	}

	refs := make([]GraphRef, 0, len(edges))
	for _, e := range edges {
		ref := GraphRef{
			Qualifier: e.DstQualifier,
			EdgeKind:  e.Kind,
			Line:      e.Line,
			RefFile:   e.FilePath,
			Depth:     1,
		}

		// The parser's SrcName is authoritative for identity: a chunk can merge
		// several definitions and reports only the first one's name, and a large
		// function's inner chunks are unnamed entirely. The chunk is used for
		// location, and for identity only where the parser had no name to give
		// (C declares no name field; C++ methods report their class).
		chunk, haveChunk := syms.byID[e.SrcID]
		ref.Name = e.SrcName
		if ref.Name == "" && haveChunk {
			ref.Name = chunk.Name
		}
		if ref.Name == "" {
			// Nothing on either side identifies the referencing definition.
			continue
		}
		ref.Resolved = true

		switch def, found := q.resolve(syms, imports, ref.Name, "", e.FilePath); {
		case found:
			ref.FilePath = def.FilePath
			ref.NodeKind = def.NodeKind
			ref.Parent = def.Parent
			ref.StartLine = def.StartLine
			ref.EndLine = def.EndLine
		case haveChunk:
			ref.FilePath = chunk.FilePath
			ref.NodeKind = chunk.NodeKind
			ref.Parent = chunk.Parent
			ref.StartLine = chunk.StartLine
			ref.EndLine = chunk.EndLine
		default:
			// Locate it by the reference site as a last resort.
			ref.FilePath = e.FilePath
			ref.StartLine = e.Line
		}

		refs = append(refs, ref)
	}
	return refs, nil
}

// resolve picks the definition a reference most likely points at.
//
// Ranking, best first:
//  1. a definition in the same file as the reference;
//  2. a definition in a file reachable from the referencing file's imports,
//     matched against the qualifier by alias or module tail;
//  3. any definition with the name.
//
// Resolution is name-based by design. Following it precisely would need type
// inference, so ambiguity is ranked rather than guessed, and the second return
// value reports only whether any candidate exists.
func (q *Querier) resolve(syms *cachedSymbols, imports map[string][]store.Import, name, qualifier, fromFile string) (store.Symbol, bool) {
	candidates := syms.byName[name]
	if len(candidates) == 0 {
		return store.Symbol{}, false
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}

	for _, c := range candidates {
		if c.FilePath == fromFile {
			return c, true
		}
	}

	if qualifier != "" {
		if module, ok := moduleForQualifier(imports[fromFile], qualifier); ok {
			for _, c := range candidates {
				if fileMatchesModule(c.FilePath, module) {
					return c, true
				}
			}
		}
	}

	return candidates[0], true
}

// moduleForQualifier finds the module a qualifier refers to, matching an
// explicit alias first and then the trailing segment of a module path
// ("fmt" for "fmt", "strings" for "str" when aliased).
func moduleForQualifier(imports []store.Import, qualifier string) (string, bool) {
	for _, im := range imports {
		if im.Alias == qualifier {
			return im.Module, true
		}
	}
	for _, im := range imports {
		if path.Base(strings.TrimSuffix(im.Module, "/")) == qualifier {
			return im.Module, true
		}
	}
	return "", false
}

// fileMatchesModule reports whether filePath plausibly belongs to module. Module
// paths are as written in source and need not correspond to on-disk layout, so
// this is a containment check on the module's trailing segments.
func fileMatchesModule(filePath, module string) bool {
	module = strings.Trim(strings.TrimSuffix(module, "/"), "./")
	if module == "" {
		return false
	}
	dir := path.Dir(filepath.ToSlash(filePath))
	return dir == module || strings.HasSuffix(dir, "/"+module)
}

// importIndex loads the imports for the given files, grouped by file.
func (q *Querier) importIndex(ctx context.Context, files []string) (map[string][]store.Import, error) {
	if len(files) == 0 {
		return nil, nil
	}
	imports, err := q.store.ImportsByFile(ctx, files)
	if err != nil {
		return nil, fmt.Errorf("loading imports: %w", err)
	}
	byFile := make(map[string][]store.Import)
	for _, im := range imports {
		byFile[im.FilePath] = append(byFile[im.FilePath], im)
	}
	return byFile, nil
}

// edgeFiles returns the distinct files the given edges live in.
func edgeFiles(edges []store.Edge) []string {
	seen := make(map[string]struct{}, len(edges))
	files := make([]string, 0, len(edges))
	for _, e := range edges {
		if _, ok := seen[e.FilePath]; ok {
			continue
		}
		seen[e.FilePath] = struct{}{}
		files = append(files, e.FilePath)
	}
	return files
}

// dedupeRefs collapses refs that point at the same place from the same line.
func dedupeRefs(refs []GraphRef) []GraphRef {
	type key struct {
		name, qualifier, kind, refFile string
		line                           int
	}
	seen := make(map[key]struct{}, len(refs))
	out := refs[:0]
	for _, r := range refs {
		k := key{r.Name, r.Qualifier, r.EdgeKind, r.RefFile, r.Line}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}

// dedupeByDefinition keeps one ref per referencing definition, discarding the
// repeat call sites within it. Used for caller-style answers, where the question
// is which definitions depend on a symbol, not how many times each does.
func dedupeByDefinition(refs []GraphRef) []GraphRef {
	type key struct{ name, file string }
	seen := make(map[key]struct{}, len(refs))
	out := refs[:0]
	for _, r := range refs {
		k := key{r.Name, r.FilePath}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, r)
	}
	return out
}

// RefFormat controls how [FormatRefs] renders a traversal result.
type RefFormat struct {
	// Relation names the relationship in the heading, e.g. "callers of".
	Relation string
	// Limit caps how many refs are printed; <= 0 prints all of them.
	Limit int
	// ShowDepth appends the traversal distance, meaningful only for Impact.
	ShowDepth bool
	// PreferRefSite selects which of a ref's two locations to print. For
	// callers and impact the useful location is where the reference occurs,
	// since that is the line to open; for callees it is the definition being
	// referenced.
	PreferRefSite bool
}

// FormatRefs renders traversal results as one line per reference, in the same
// file:line shape the search results' metadata mode uses. Rendering lives here
// so the CLI and the MCP server cannot drift apart, matching how [Querier.Map]
// already owns the repo map's presentation.
func FormatRefs(refs []GraphRef, symbol string, f RefFormat) string {
	if len(refs) == 0 {
		return fmt.Sprintf("No %s %s found.\n", f.Relation, symbol)
	}

	truncated := false
	if f.Limit > 0 && len(refs) > f.Limit {
		refs = refs[:f.Limit]
		truncated = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d %s %s:\n", len(refs), f.Relation, symbol)
	for _, r := range refs {
		b.WriteString("  " + formatRef(r, f.PreferRefSite))
		if r.EdgeKind != "" && r.EdgeKind != edgeKindCall {
			fmt.Fprintf(&b, " (%s)", r.EdgeKind)
		}
		if f.ShowDepth && r.Depth > 0 {
			fmt.Fprintf(&b, " depth=%d", r.Depth)
		}
		b.WriteString("\n")
	}
	if truncated {
		fmt.Fprintf(&b, "\n(truncated to %d; raise the limit to see more)\n", f.Limit)
	}
	return b.String()
}

// formatRef renders one ref as "file:line name", marking targets that have no
// definition in the index and qualifying a name by its containing symbol.
func formatRef(r GraphRef, preferRefSite bool) string {
	if !r.Resolved {
		name := r.Name
		if r.Qualifier != "" {
			name = r.Qualifier + "." + r.Name
		}
		return fmt.Sprintf("%s:%d %s (external)", r.RefFile, r.Line, name)
	}

	file, line := r.FilePath, r.StartLine
	if preferRefSite && r.RefFile != "" {
		file, line = r.RefFile, r.Line
	}

	name := r.Name
	if r.Parent != "" {
		name = r.Parent + "." + r.Name
	}
	return fmt.Sprintf("%s:%d %s", file, line, name)
}

// emptyReason distinguishes "the graph was never built" from "this symbol has no
// relationships", returning ErrGraphNotBuilt only in the former case. The edge
// count is consulted only when there is nothing to return, keeping the COUNT(*)
// off the common path.
func (q *Querier) emptyReason(ctx context.Context) error {
	count, err := q.store.EdgeCount(ctx)
	if err != nil {
		return fmt.Errorf("counting edges: %w", err)
	}
	if count == 0 {
		return ErrGraphNotBuilt
	}
	return nil
}
