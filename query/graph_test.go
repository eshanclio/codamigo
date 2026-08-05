package query_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ieshan/codamigo/query"
	"github.com/ieshan/codamigo/store"
)

// graphSetup builds a store from the given file entries and returns a Querier.
func graphSetup(t *testing.T, entries []store.FileRecords) (*query.Querier, store.Store) {
	t.Helper()
	s, err := store.NewSQLiteStore(t.TempDir()+"/test.db", "test-model", 3)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if len(entries) > 0 {
		if err = s.ReplaceByFiles(t.Context(), entries); err != nil {
			t.Fatalf("ReplaceByFiles: %v", err)
		}
	}
	return query.New(&fakeEmbedder{vec: []float32{1, 0, 0}}, s), s
}

func gRec(id, path, name string, start, end int) store.Record {
	return store.Record{
		ID: id, FilePath: path, Language: "go",
		Content: "body of " + name, ContentHash: "h-" + id,
		NodeKind: "function_declaration", Name: name,
		StartLine: start, EndLine: end,
		Embedding: []float32{1, 0, 0},
	}
}

// A call in main.go to a function defined in helper.go must resolve across files.
func TestCallers_ResolvesCrossFile(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "main.go", FileHash: "h1",
			Records: []store.Record{gRec("m1", "main.go", "Run", 1, 10)},
			Edges: []store.Edge{
				{SrcID: "m1", Kind: "call", DstName: "Helper", Line: 5},
			},
		},
		{
			FilePath: "helper.go", FileHash: "h2",
			Records: []store.Record{gRec("h1c", "helper.go", "Helper", 1, 8)},
		},
	})

	refs, err := q.Callers(t.Context(), "Helper")
	if err != nil {
		t.Fatalf("Callers: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 caller, got %d: %+v", len(refs), refs)
	}
	if refs[0].Name != "Run" {
		t.Errorf("caller = %q, want Run", refs[0].Name)
	}
	if refs[0].FilePath != "main.go" || refs[0].Line != 5 {
		t.Errorf("unexpected caller location: %+v", refs[0])
	}
	if !refs[0].Resolved {
		t.Error("a caller found in the index should be marked resolved")
	}
}

func TestCallees_IncludesUnresolvedTargets(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "main.go", FileHash: "h1",
			Records: []store.Record{gRec("m1", "main.go", "Run", 1, 10)},
			Edges: []store.Edge{
				{SrcID: "m1", Kind: "call", DstName: "Helper", Line: 4},
				// Not defined anywhere in the index (third-party).
				{SrcID: "m1", Kind: "call", DstName: "Println", DstQualifier: "fmt", Line: 5},
			},
		},
		{
			FilePath: "helper.go", FileHash: "h2",
			Records: []store.Record{gRec("h1c", "helper.go", "Helper", 1, 8)},
		},
	})

	refs, err := q.Callees(t.Context(), "Run")
	if err != nil {
		t.Fatalf("Callees: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected 2 callees, got %d: %+v", len(refs), refs)
	}

	byName := map[string]query.GraphRef{}
	for _, r := range refs {
		byName[r.Name] = r
	}

	helper, ok := byName["Helper"]
	if !ok || !helper.Resolved {
		t.Errorf("Helper should resolve to its definition: %+v", helper)
	}
	if helper.FilePath != "helper.go" {
		t.Errorf("Helper resolved to %q, want helper.go", helper.FilePath)
	}

	println, ok := byName["Println"]
	if !ok {
		t.Fatal("expected an unresolved Println callee")
	}
	if println.Resolved {
		t.Error("Println is not in the index and must be unresolved")
	}
	if println.Qualifier != "fmt" {
		t.Errorf("qualifier = %q, want fmt", println.Qualifier)
	}
}

// With two same-named definitions, a reference in the same file wins.
func TestResolve_PrefersSameFile(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{
				gRec("a1", "a.go", "Caller", 1, 10),
				gRec("a2", "a.go", "Target", 11, 20),
			},
			Edges: []store.Edge{
				{SrcID: "a1", Kind: "call", DstName: "Target", Line: 5},
			},
		},
		{
			FilePath: "b.go", FileHash: "h2",
			Records: []store.Record{gRec("b1", "b.go", "Target", 1, 8)},
		},
	})

	refs, err := q.Callees(t.Context(), "Caller")
	if err != nil {
		t.Fatalf("Callees: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 callee, got %+v", refs)
	}
	if refs[0].FilePath != "a.go" {
		t.Errorf("expected the same-file definition, got %q", refs[0].FilePath)
	}
}

// A qualifier that matches an import alias should steer resolution to that module.
func TestResolve_UsesImportQualifier(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "main.go", FileHash: "h1",
			Records: []store.Record{gRec("m1", "main.go", "Run", 1, 10)},
			Edges: []store.Edge{
				{SrcID: "m1", Kind: "call", DstName: "Get", DstQualifier: "st", Line: 5},
			},
			Imports: []store.Import{
				{Module: "store", Alias: "st", Line: 2},
			},
		},
		{
			FilePath: "store/store.go", FileHash: "h2",
			Records: []store.Record{gRec("s1", "store/store.go", "Get", 1, 8)},
		},
		{
			FilePath: "cache/cache.go", FileHash: "h3",
			Records: []store.Record{gRec("c1", "cache/cache.go", "Get", 1, 8)},
		},
	})

	refs, err := q.Callees(t.Context(), "Run")
	if err != nil {
		t.Fatalf("Callees: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 callee, got %+v", refs)
	}
	if refs[0].FilePath != "store/store.go" {
		t.Errorf("alias 'st' should resolve to store/, got %q", refs[0].FilePath)
	}
}

func TestImpact_TransitiveAndBounded(t *testing.T) {
	// C calls B, B calls A. Impact of A at depth 2 reaches both B and C.
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{gRec("a1", "a.go", "A", 1, 5)},
		},
		{
			FilePath: "b.go", FileHash: "h2",
			Records: []store.Record{gRec("b1", "b.go", "B", 1, 5)},
			Edges:   []store.Edge{{SrcID: "b1", Kind: "call", DstName: "A", Line: 2}},
		},
		{
			FilePath: "c.go", FileHash: "h3",
			Records: []store.Record{gRec("c1", "c.go", "C", 1, 5)},
			Edges:   []store.Edge{{SrcID: "c1", Kind: "call", DstName: "B", Line: 2}},
		},
	})
	ctx := t.Context()

	deep, err := q.Impact(ctx, "A", 2)
	if err != nil {
		t.Fatalf("Impact depth 2: %v", err)
	}
	names := map[string]int{}
	for _, r := range deep {
		names[r.Name] = r.Depth
	}
	if names["B"] != 1 {
		t.Errorf("B should be at depth 1, got %d (%+v)", names["B"], deep)
	}
	if names["C"] != 2 {
		t.Errorf("C should be at depth 2, got %d (%+v)", names["C"], deep)
	}

	// Depth 1 stops at direct callers.
	shallow, err := q.Impact(ctx, "A", 1)
	if err != nil {
		t.Fatalf("Impact depth 1: %v", err)
	}
	if len(shallow) != 1 || shallow[0].Name != "B" {
		t.Errorf("depth 1 should yield only B, got %+v", shallow)
	}
}

// A mutually recursive pair must not loop forever.
func TestImpact_TerminatesOnCycle(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{gRec("a1", "a.go", "A", 1, 5)},
			Edges:   []store.Edge{{SrcID: "a1", Kind: "call", DstName: "B", Line: 2}},
		},
		{
			FilePath: "b.go", FileHash: "h2",
			Records: []store.Record{gRec("b1", "b.go", "B", 1, 5)},
			Edges:   []store.Edge{{SrcID: "b1", Kind: "call", DstName: "A", Line: 2}},
		},
	})

	refs, err := q.Impact(t.Context(), "A", 5)
	if err != nil {
		t.Fatalf("Impact: %v", err)
	}
	// A is the origin and is already visited, so only B is reported.
	if len(refs) != 1 || refs[0].Name != "B" {
		t.Errorf("expected only B, got %+v", refs)
	}
}

// An index with chunks but no edges means the graph was never built; that must
// be distinguishable from a symbol simply having no callers.
func TestGraph_NotBuiltIsDistinctFromNoResults(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{gRec("a1", "a.go", "A", 1, 5)},
		},
	})

	_, err := q.Callers(t.Context(), "A")
	if !errors.Is(err, query.ErrGraphNotBuilt) {
		t.Errorf("expected ErrGraphNotBuilt, got %v", err)
	}
}

func TestGraph_NoCallersWhenGraphExists(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{
				gRec("a1", "a.go", "A", 1, 5),
				gRec("a2", "a.go", "Lonely", 6, 10),
			},
			Edges: []store.Edge{{SrcID: "a1", Kind: "call", DstName: "Something", Line: 2}},
		},
	})

	refs, err := q.Callers(t.Context(), "Lonely")
	if err != nil {
		t.Errorf("a symbol with no callers is not an error, got %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected no callers, got %+v", refs)
	}
}

// The symbol index is generation-cached; re-indexing must be reflected.
func TestGraph_CacheInvalidation(t *testing.T) {
	q, s := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{gRec("a1", "a.go", "A", 1, 5)},
			Edges:   []store.Edge{{SrcID: "a1", Kind: "call", DstName: "Target", Line: 2}},
		},
	})
	ctx := t.Context()

	// Prime the cache: Target is not yet defined anywhere.
	refs, err := q.Callees(ctx, "A")
	if err != nil {
		t.Fatalf("Callees: %v", err)
	}
	if len(refs) != 1 || refs[0].Resolved {
		t.Fatalf("expected an unresolved target initially, got %+v", refs)
	}

	// Add the definition and invalidate, as the indexer does.
	if err = s.ReplaceByFiles(ctx, []store.FileRecords{{
		FilePath: "b.go", FileHash: "h2",
		Records: []store.Record{gRec("b1", "b.go", "Target", 1, 5)},
	}}); err != nil {
		t.Fatalf("ReplaceByFiles: %v", err)
	}
	q.InvalidateMapCache()

	refs, err = q.Callees(ctx, "A")
	if err != nil {
		t.Fatalf("Callees after invalidation: %v", err)
	}
	if len(refs) != 1 || !refs[0].Resolved {
		t.Errorf("expected the target to resolve after re-index, got %+v", refs)
	}
}

// A function too large to fit one chunk produces unnamed inner chunks, and some
// references fall outside every chunk's span. The parser's SrcName must still
// identify the calling definition in both cases.
func TestCallers_AttributedByParserNameWhenChunkIsUnnamed(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "big.go", FileHash: "h1",
			Records: []store.Record{
				// The chunk covering line 50 exists but carries no symbol name.
				{
					ID: "frag", FilePath: "big.go", Language: "go",
					Content: "fragment", ContentHash: "hf", NodeKind: "block",
					Name: "", StartLine: 40, EndLine: 60,
					Embedding: []float32{1, 0, 0},
				},
			},
			Edges: []store.Edge{
				{SrcID: "frag", SrcName: "HugeFunc", Kind: "call", DstName: "Target", Line: 50},
				// No chunk covers line 900; only SrcName identifies the caller.
				{SrcID: "", SrcName: "OtherFunc", Kind: "call", DstName: "Target", Line: 900},
			},
		},
		{
			FilePath: "t.go", FileHash: "h2",
			Records: []store.Record{gRec("t1", "t.go", "Target", 1, 5)},
		},
	})

	refs, err := q.Callers(t.Context(), "Target")
	if err != nil {
		t.Fatalf("Callers: %v", err)
	}

	names := map[string]bool{}
	for _, r := range refs {
		names[r.Name] = true
	}
	for _, want := range []string{"HugeFunc", "OtherFunc"} {
		if !names[want] {
			t.Errorf("expected %s among callers, got %+v", want, refs)
		}
	}
}

// A caller that references the symbol several times is one caller.
func TestCallers_DedupedByDefinition(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{gRec("a1", "a.go", "Caller", 1, 20)},
			Edges: []store.Edge{
				{SrcID: "a1", SrcName: "Caller", Kind: "call", DstName: "Target", Line: 5},
				{SrcID: "a1", SrcName: "Caller", Kind: "call", DstName: "Target", Line: 6},
				{SrcID: "a1", SrcName: "Caller", Kind: "call", DstName: "Target", Line: 7},
			},
		},
		{
			FilePath: "t.go", FileHash: "h2",
			Records: []store.Record{gRec("t1", "t.go", "Target", 1, 5)},
		},
	})

	refs, err := q.Callers(t.Context(), "Target")
	if err != nil {
		t.Fatalf("Callers: %v", err)
	}
	if len(refs) != 1 {
		t.Errorf("three call sites in one function is one caller, got %d: %+v", len(refs), refs)
	}
}

// Unresolved type references are builtins or stdlib types and are dropped;
// unresolved calls are kept because a call into a dependency is informative.
func TestCallees_DropsUnresolvedTypeReferences(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{gRec("a1", "a.go", "Fn", 1, 10)},
			Edges: []store.Edge{
				{SrcID: "a1", SrcName: "Fn", Kind: "reference", DstName: "string", Line: 2},
				{SrcID: "a1", SrcName: "Fn", Kind: "reference", DstName: "error", Line: 3},
				{SrcID: "a1", SrcName: "Fn", Kind: "call", DstName: "Println", DstQualifier: "fmt", Line: 4},
			},
		},
	})

	refs, err := q.Callees(t.Context(), "Fn")
	if err != nil {
		t.Fatalf("Callees: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected only the unresolved call, got %+v", refs)
	}
	if refs[0].Name != "Println" {
		t.Errorf("expected the fmt.Println call to survive, got %+v", refs[0])
	}
}

// A resolved type reference is a real project dependency and must be kept.
func TestCallees_KeepsResolvedTypeReferences(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{gRec("a1", "a.go", "Fn", 1, 10)},
			Edges: []store.Edge{
				{SrcID: "a1", SrcName: "Fn", Kind: "reference", DstName: "Config", Line: 2},
			},
		},
		{
			FilePath: "cfg.go", FileHash: "h2",
			Records: []store.Record{gRec("c1", "cfg.go", "Config", 1, 5)},
		},
	})

	refs, err := q.Callees(t.Context(), "Fn")
	if err != nil {
		t.Fatalf("Callees: %v", err)
	}
	if len(refs) != 1 || refs[0].Name != "Config" || !refs[0].Resolved {
		t.Errorf("a resolved type reference should be reported, got %+v", refs)
	}
}

func TestGraph_UnknownSymbol(t *testing.T) {
	q, _ := graphSetup(t, []store.FileRecords{
		{
			FilePath: "a.go", FileHash: "h1",
			Records: []store.Record{gRec("a1", "a.go", "A", 1, 5)},
			Edges:   []store.Edge{{SrcID: "a1", Kind: "call", DstName: "B", Line: 2}},
		},
	})
	ctx := t.Context()

	refs, err := q.Callees(ctx, "DoesNotExist")
	if err != nil {
		t.Errorf("unknown symbol should not error, got %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected no callees, got %+v", refs)
	}
}

// FormatRefs is the single renderer shared by the CLI commands and the MCP
// tools, so its output contract is worth pinning down directly.
func TestFormatRefs(t *testing.T) {
	resolved := query.GraphRef{
		Name: "Helper", EdgeKind: "call", Resolved: true,
		FilePath: "helper.go", StartLine: 3, EndLine: 9,
		RefFile: "main.go", Line: 42, Depth: 1,
	}

	tests := []struct {
		name    string
		refs    []query.GraphRef
		format  query.RefFormat
		want    []string
		notWant []string
	}{
		{
			name:   "empty is a sentence, not a blank line",
			refs:   nil,
			format: query.RefFormat{Relation: "callers of"},
			want:   []string{"No callers of Helper found."},
		},
		{
			name:   "callers point at the reference site",
			refs:   []query.GraphRef{resolved},
			format: query.RefFormat{Relation: "callers of", PreferRefSite: true},
			want:   []string{"1 callers of Helper:", "main.go:42 Helper"},
		},
		{
			name:   "callees point at the definition",
			refs:   []query.GraphRef{resolved},
			format: query.RefFormat{Relation: "referenced by"},
			want:   []string{"helper.go:3 Helper"},
		},
		{
			name: "a method is qualified by its parent once",
			refs: []query.GraphRef{{
				Name: "Search", Parent: "Store", EdgeKind: "call", Resolved: true,
				FilePath: "store/store.go", StartLine: 7,
			}},
			format:  query.RefFormat{Relation: "callers of"},
			want:    []string{"store/store.go:7 Store.Search"},
			notWant: []string{"Store.Search Store.Search", "[Store.Search]"},
		},
		{
			name: "an unresolved target is marked external and keeps its qualifier",
			refs: []query.GraphRef{{
				Name: "Println", Qualifier: "fmt", EdgeKind: "call",
				RefFile: "main.go", Line: 5,
			}},
			format: query.RefFormat{Relation: "referenced by"},
			want:   []string{"main.go:5 fmt.Println (external)"},
		},
		{
			name: "a non-call relationship is labelled",
			refs: []query.GraphRef{{
				Name: "Config", EdgeKind: "reference", Resolved: true,
				FilePath: "config/config.go", StartLine: 12,
			}},
			format: query.RefFormat{Relation: "referenced by"},
			want:   []string{"config/config.go:12 Config (reference)"},
		},
		{
			name:   "depth is reported only when asked for",
			refs:   []query.GraphRef{resolved},
			format: query.RefFormat{Relation: "affected by changing", ShowDepth: true, PreferRefSite: true},
			want:   []string{"depth=1"},
		},
		{
			name:    "depth is omitted otherwise",
			refs:    []query.GraphRef{resolved},
			format:  query.RefFormat{Relation: "callers of", PreferRefSite: true},
			notWant: []string{"depth="},
		},
		{
			name:   "over the limit the list is trimmed and the trim is announced",
			refs:   []query.GraphRef{resolved, resolved, resolved},
			format: query.RefFormat{Relation: "callers of", Limit: 2, PreferRefSite: true},
			want:   []string{"2 callers of Helper:", "truncated to 2"},
		},
		{
			name:    "a zero limit means no limit",
			refs:    []query.GraphRef{resolved, resolved, resolved},
			format:  query.RefFormat{Relation: "callers of"},
			want:    []string{"3 callers of Helper:"},
			notWant: []string{"truncated"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := query.FormatRefs(tc.refs, "Helper", tc.format)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q:\n%s", want, got)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("output should not contain %q:\n%s", notWant, got)
				}
			}
			if !strings.HasSuffix(got, "\n") {
				t.Errorf("output should end in a newline so callers can print it as-is:\n%q", got)
			}
		})
	}
}
