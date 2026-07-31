package indexer

import (
	"cmp"
	"slices"
	"sort"

	"github.com/ieshan/go-code-chunker/chunker"

	"github.com/ieshan/codamigo/store"
)

// mapEdges translates chunker edges into their store representations for one
// file. It is the only place chunker.Edge crosses into store types, mirroring
// the chunk → Record mapping.
//
// Import edges carry no enclosing definition and become file-scoped Imports.
// Every other edge is attributed to the chunk whose line span contains the
// reference. Attribution is by line rather than by the edge's Source name
// because a chunk may merge several definitions, and because some languages
// report a coarse or empty Source (C has no name field; C++ methods report
// their enclosing class).
func mapEdges(filePath string, edges []chunker.Edge, records []store.Record) ([]store.Edge, []store.Import) {
	if len(edges) == 0 {
		return nil, nil
	}

	var (
		out     []store.Edge
		imports []store.Import
	)
	locator := newChunkLocator(records)

	for _, e := range edges {
		if e.Kind == chunker.EdgeImport {
			imports = append(imports, store.Import{
				FilePath: filePath,
				Module:   e.Target,
				Alias:    e.TargetQualifier,
				Line:     e.Line,
			})
			continue
		}

		// A reference may fall outside every chunk's span, since chunking is
		// driven by size rather than coverage. Such an edge keeps an empty
		// SrcID — it cannot answer "what does this chunk reference?" — but the
		// parser's Source still identifies the enclosing definition, so it can
		// still answer "what references this symbol?".
		srcID, ok := locator.at(e.Line)
		if !ok && e.Source == "" {
			continue
		}
		out = append(out, store.Edge{
			SrcID:    srcID,
			FilePath: filePath,
			// The parser's own view of the enclosing definition is kept because
			// a large function can be split across chunks, leaving the chunk
			// holding this reference unnamed.
			SrcName:      e.Source,
			Kind:         string(e.Kind),
			DstName:      e.Target,
			DstQualifier: e.TargetQualifier,
			Line:         e.Line,
		})
	}

	return out, imports
}

// chunkSpan is one chunk's line range and identity.
type chunkSpan struct {
	start, end int
	id         string
}

// chunkLocator maps a source line to the chunk covering it.
type chunkLocator struct {
	spans []chunkSpan // ascending by start line
}

// newChunkLocator indexes records by line span. Records are usually already in
// line order, but the input order is not guaranteed so they are sorted. The sort
// is stable so that equal start lines keep their original relative order, which
// [chunkLocator.at] relies on to pick the innermost chunk.
func newChunkLocator(records []store.Record) *chunkLocator {
	spans := make([]chunkSpan, len(records))
	for i, r := range records {
		spans[i] = chunkSpan{start: r.StartLine, end: r.EndLine, id: r.ID}
	}
	slices.SortStableFunc(spans, func(a, b chunkSpan) int {
		return cmp.Compare(a.start, b.start)
	})
	return &chunkLocator{spans: spans}
}

// at returns the ID of the chunk covering line, if any. When spans overlap it
// returns the last chunk that starts at or before the line, which is the
// innermost enclosing one.
func (l *chunkLocator) at(line int) (string, bool) {
	// Rightmost chunk whose start is <= line. slices has no predicate-based
	// binary search, so sort.Search is still the right tool here.
	i := sort.Search(len(l.spans), func(i int) bool { return l.spans[i].start > line }) - 1
	for ; i >= 0; i-- {
		if line <= l.spans[i].end {
			return l.spans[i].id, true
		}
	}
	return "", false
}
