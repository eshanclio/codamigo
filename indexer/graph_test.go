package indexer

import (
	"testing"

	"github.com/ieshan/go-code-chunker/chunker"

	"github.com/ieshan/codamigo/store"
)

func rec(id string, start, end int) store.Record {
	return store.Record{ID: id, StartLine: start, EndLine: end}
}

func TestMapEdges_AttributesByLineSpan(t *testing.T) {
	records := []store.Record{
		rec("chunk-a", 1, 10),
		rec("chunk-b", 11, 20),
	}
	edges := []chunker.Edge{
		{Kind: chunker.EdgeCall, Target: "Helper", Line: 5},
		{Kind: chunker.EdgeCall, Target: "Other", Line: 15},
	}

	out, imports := mapEdges("a.go", edges, records)

	if len(imports) != 0 {
		t.Errorf("expected no imports, got %+v", imports)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(out))
	}
	if out[0].SrcID != "chunk-a" {
		t.Errorf("line 5 should attribute to chunk-a, got %q", out[0].SrcID)
	}
	if out[1].SrcID != "chunk-b" {
		t.Errorf("line 15 should attribute to chunk-b, got %q", out[1].SrcID)
	}
	if out[0].FilePath != "a.go" || out[0].Kind != "call" || out[0].DstName != "Helper" {
		t.Errorf("unexpected mapped edge: %+v", out[0])
	}
}

func TestMapEdges_ImportsBecomeFileScoped(t *testing.T) {
	records := []store.Record{rec("chunk-a", 1, 10)}
	edges := []chunker.Edge{
		{Kind: chunker.EdgeImport, Target: "strings", TargetQualifier: "str", Line: 3},
		{Kind: chunker.EdgeImport, Target: "fmt", Line: 2},
	}

	out, imports := mapEdges("a.go", edges, records)

	if len(out) != 0 {
		t.Errorf("imports must not produce symbol edges, got %+v", out)
	}
	if len(imports) != 2 {
		t.Fatalf("expected 2 imports, got %d", len(imports))
	}
	if imports[0].Module != "strings" || imports[0].Alias != "str" {
		t.Errorf("alias should map to Alias: %+v", imports[0])
	}
	if imports[0].FilePath != "a.go" {
		t.Errorf("import should carry the file path, got %q", imports[0].FilePath)
	}
}

func TestMapEdges_QualifierPreserved(t *testing.T) {
	records := []store.Record{rec("chunk-a", 1, 10)}
	edges := []chunker.Edge{
		{Kind: chunker.EdgeCall, Target: "Println", TargetQualifier: "fmt", Line: 4},
	}

	out, _ := mapEdges("a.go", edges, records)
	if len(out) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(out))
	}
	if out[0].DstQualifier != "fmt" || out[0].DstName != "Println" {
		t.Errorf("qualifier not preserved: %+v", out[0])
	}
}

// A reference on a line no chunk covers cannot be anchored and is dropped
// rather than stored against a wrong or empty source.
func TestMapEdges_DropsUnanchoredReferences(t *testing.T) {
	records := []store.Record{rec("chunk-a", 10, 20)}
	edges := []chunker.Edge{
		{Kind: chunker.EdgeCall, Target: "Orphan", Line: 2},
		{Kind: chunker.EdgeCall, Target: "Anchored", Line: 12},
	}

	out, _ := mapEdges("a.go", edges, records)
	if len(out) != 1 {
		t.Fatalf("expected only the anchored edge, got %+v", out)
	}
	if out[0].DstName != "Anchored" {
		t.Errorf("kept the wrong edge: %+v", out[0])
	}
}

// When a nested chunk sits inside an outer one, the innermost chunk wins.
func TestMapEdges_InnermostChunkWins(t *testing.T) {
	records := []store.Record{
		rec("outer", 1, 30),
		rec("inner", 10, 20),
	}
	edges := []chunker.Edge{{Kind: chunker.EdgeCall, Target: "X", Line: 15}}

	out, _ := mapEdges("a.go", edges, records)
	if len(out) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(out))
	}
	if out[0].SrcID != "inner" {
		t.Errorf("expected innermost chunk, got %q", out[0].SrcID)
	}
}

// Records arriving out of line order must still attribute correctly.
func TestMapEdges_UnorderedRecords(t *testing.T) {
	records := []store.Record{
		rec("chunk-c", 21, 30),
		rec("chunk-a", 1, 10),
		rec("chunk-b", 11, 20),
	}
	edges := []chunker.Edge{{Kind: chunker.EdgeCall, Target: "X", Line: 25}}

	out, _ := mapEdges("a.go", edges, records)
	if len(out) != 1 || out[0].SrcID != "chunk-c" {
		t.Errorf("expected chunk-c, got %+v", out)
	}
}

func TestMapEdges_NoEdgesOrNoRecords(t *testing.T) {
	if out, imports := mapEdges("a.go", nil, []store.Record{rec("a", 1, 5)}); out != nil || imports != nil {
		t.Errorf("nil edges should map to nil, got %+v / %+v", out, imports)
	}

	// Imports still apply when a file produced no chunks at all.
	edges := []chunker.Edge{{Kind: chunker.EdgeImport, Target: "fmt", Line: 1}}
	out, imports := mapEdges("a.go", edges, nil)
	if len(out) != 0 {
		t.Errorf("expected no symbol edges, got %+v", out)
	}
	if len(imports) != 1 {
		t.Errorf("imports should survive with no chunks, got %+v", imports)
	}
}

// Boundary lines belong to their chunk.
func TestMapEdges_SpanBoundariesInclusive(t *testing.T) {
	records := []store.Record{rec("chunk-a", 5, 9)}
	for _, line := range []int{5, 9} {
		out, _ := mapEdges("a.go", []chunker.Edge{
			{Kind: chunker.EdgeCall, Target: "X", Line: line},
		}, records)
		if len(out) != 1 {
			t.Errorf("line %d should be inside the span, got %+v", line, out)
		}
	}
	for _, line := range []int{4, 10} {
		out, _ := mapEdges("a.go", []chunker.Edge{
			{Kind: chunker.EdgeCall, Target: "X", Line: line},
		}, records)
		if len(out) != 0 {
			t.Errorf("line %d should be outside the span, got %+v", line, out)
		}
	}
}
