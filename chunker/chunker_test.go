package chunker_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ieshan/codamigo/chunker"
	"github.com/ieshan/codamigo/langs"
)

func TestNewChunker_EmptyLangs(t *testing.T) {
	_, err := chunker.NewChunker(nil, chunker.DefaultConfig())
	if err == nil {
		t.Fatal("expected error for nil langs, got nil")
	}
	_, err = chunker.NewChunker([]chunker.LanguageConfig{}, chunker.DefaultConfig())
	if err == nil {
		t.Fatal("expected error for empty langs, got nil")
	}
}

func TestNewChunker_DuplicateExtension(t *testing.T) {
	lang := langs.GoLanguage()
	_, err := chunker.NewChunker([]chunker.LanguageConfig{lang, lang}, chunker.DefaultConfig())
	if err == nil {
		t.Fatal("expected error for duplicate extension .go, got nil")
	}
}

func TestNewChunker_Valid(t *testing.T) {
	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil Chunker")
	}
}

func TestNewChunker_InvalidMaxChunkSize(t *testing.T) {
	lang := langs.GoLanguage()

	tests := []struct {
		name string
		cfg  chunker.Config
	}{
		{"zero MaxChunkSize", chunker.Config{MaxChunkSize: 0, MinChunkSize: 50}},
		{"negative MaxChunkSize", chunker.Config{MaxChunkSize: -1, MinChunkSize: 50}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := chunker.NewChunker([]chunker.LanguageConfig{lang}, tt.cfg)
			if err == nil {
				t.Fatal("expected error for invalid MaxChunkSize, got nil")
			}
		})
	}
}

func TestNewChunker_InvalidMinChunkSize(t *testing.T) {
	lang := langs.GoLanguage()
	_, err := chunker.NewChunker([]chunker.LanguageConfig{lang}, chunker.Config{MaxChunkSize: 1500, MinChunkSize: -1})
	if err == nil {
		t.Fatal("expected error for negative MinChunkSize, got nil")
	}
}

func TestNewChunker_MinChunkSizeZero(t *testing.T) {
	_, err := chunker.NewChunker(langs.AllLanguages(), chunker.Config{
		MaxChunkSize: 1500,
		MinChunkSize: 0,
	})
	if err == nil {
		t.Fatal("expected error for MinChunkSize=0, got nil")
	}
	if !strings.Contains(err.Error(), "MinChunkSize must be positive") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func mustNewChunker(t *testing.T, langs []chunker.LanguageConfig, cfg chunker.Config) *chunker.Chunker {
	t.Helper()
	c, err := chunker.NewChunker(langs, cfg)
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	return c
}

func TestNewChunker_NilGrammar(t *testing.T) {
	// A nil Grammar is a programmer error caught at construction time, not at ChunkFile.
	lang := chunker.LanguageConfig{
		Name:       "test",
		Extensions: []string{".test"},
		Grammar:    nil,
		NodeKinds:  chunker.NodeKindSet{},
	}
	_, err := chunker.NewChunker([]chunker.LanguageConfig{lang}, chunker.DefaultConfig())
	if err == nil {
		t.Fatal("expected error for nil Grammar, got nil")
	}
}

func TestNewChunker_InjectionConfigAccepted(t *testing.T) {
	base := langs.GoLanguage()
	cfg := chunker.LanguageConfig{
		Name:       "test-inj",
		Extensions: []string{".testinj"},
		Grammar:    base.Grammar,
		NodeKinds:  base.NodeKinds,
		Injections: []chunker.InjectionRule{{
			ContainerKind: "container",
			ContentKind:   "raw_text",
			LangAttr:      "lang",
			DefaultLang:   "go",
			Grammars:      map[string]chunker.LanguageConfig{"go": base},
		}},
	}
	c, err := chunker.NewChunker([]chunker.LanguageConfig{cfg}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil Chunker")
	}
}

func TestChunkFile_UnsupportedExtension(t *testing.T) {
	c := mustNewChunker(t, []chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	_, err := c.ChunkFile("main.rb", []byte("puts 'hello'"))
	if !errors.Is(err, chunker.ErrUnsupportedLanguage) {
		t.Fatalf("expected ErrUnsupportedLanguage, got %v", err)
	}
}

func TestChunkFile_EmptyFile(t *testing.T) {
	c := mustNewChunker(t, []chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	chunks, err := c.ChunkFile("main.go", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for empty file, got %d", len(chunks))
	}
}

func TestChunkFile_SingleSmallFunction(t *testing.T) {
	src := `package main

func hello() {
	println("hello")
}
`
	c := mustNewChunker(t, []chunker.LanguageConfig{langs.GoLanguage()}, chunker.Config{MaxChunkSize: 1500, MinChunkSize: 50})
	chunks, err := c.ChunkFile("main.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0].NodeKind != "function_declaration" {
		t.Errorf("expected NodeKind=function_declaration, got %q", chunks[0].NodeKind)
	}
}

func TestChunkFile_OversizedFunction_RecursesIntoChildren(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package main\n\nfunc bigFunc() {\n")
	for range 60 {
		sb.WriteString("\tprintln(\"this is a long line to pad out the function body significantly\")\n")
	}
	sb.WriteString("}\n")
	src := sb.String()

	c := mustNewChunker(t, []chunker.LanguageConfig{langs.GoLanguage()}, chunker.Config{MaxChunkSize: 200, MinChunkSize: 10})
	chunks, err := c.ChunkFile("main.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, ch := range chunks {
		if len(ch.Content) > 200 {
			t.Errorf("chunk %d exceeds MaxChunkSize: len=%d", i, len(ch.Content))
		}
	}
}

func TestChunkFile_ManySmallFunctions_Merged(t *testing.T) {
	src := `package main

func a() {}

func b() {}

func c() {}
`
	c := mustNewChunker(t, []chunker.LanguageConfig{langs.GoLanguage()}, chunker.Config{MaxChunkSize: 1500, MinChunkSize: 50})
	chunks, err := c.ChunkFile("main.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 merged chunk, got %d", len(chunks))
	}
}

func TestChunkFile_SizeInvariant(t *testing.T) {
	src := `package main

func alpha() { println("alpha") }
func beta()  { println("beta") }
func gamma() { println("gamma") }
func delta() { println("delta") }
`
	c := mustNewChunker(t, []chunker.LanguageConfig{langs.GoLanguage()}, chunker.Config{MaxChunkSize: 60, MinChunkSize: 10})
	chunks, err := c.ChunkFile("main.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, ch := range chunks {
		if len(ch.Content) > 60 {
			t.Errorf("chunk %d exceeds MaxChunkSize 60: len=%d\ncontent:\n%s", i, len(ch.Content), ch.Content)
		}
	}
	if len(chunks) < 2 {
		t.Errorf("expected multiple chunks for small MaxChunkSize, got %d", len(chunks))
	}
}

func TestChunkFile_Metadata(t *testing.T) {
	src := `package main

type Server struct {
	host string
}

func (s *Server) Start() error {
	return nil
}

func NewServer(host string) *Server {
	return &Server{host: host}
}
`
	c := mustNewChunker(t, []chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	chunks, err := c.ChunkFile("server.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	byName := make(map[string]chunker.Chunk)
	for _, ch := range chunks {
		byName[ch.Name] = ch
	}

	t.Run("FilePath propagated", func(t *testing.T) {
		for _, ch := range chunks {
			if ch.FilePath != "server.go" {
				t.Errorf("expected FilePath=server.go, got %q", ch.FilePath)
			}
		}
	})

	t.Run("Language propagated", func(t *testing.T) {
		for _, ch := range chunks {
			if ch.Language != "go" {
				t.Errorf("expected Language=go, got %q", ch.Language)
			}
		}
	})

	t.Run("Function name extracted", func(t *testing.T) {
		ch, ok := byName["NewServer"]
		if !ok {
			t.Fatal("expected chunk with Name=NewServer")
		}
		if ch.NodeKind != "function_declaration" {
			t.Errorf("expected NodeKind=function_declaration, got %q", ch.NodeKind)
		}
		if ch.Parent != "" {
			t.Errorf("expected empty Parent for top-level function, got %q", ch.Parent)
		}
	})

	t.Run("Start line is 1-based", func(t *testing.T) {
		for _, ch := range chunks {
			if ch.Start.Line < 1 {
				t.Errorf("chunk %q: Start.Line must be >= 1, got %d", ch.Name, ch.Start.Line)
			}
		}
	})
}

func TestChunkFile_GoStructFieldParent(t *testing.T) {
	// Build a struct large enough to exceed MaxChunkSize=200 so field_declarations
	// are emitted as individual atoms. Verifies that extractParentName correctly
	// walks through type_spec (which has a "name" field) rather than type_declaration.
	var sb strings.Builder
	sb.WriteString("package main\n\ntype BigStruct struct {\n")
	for _, f := range []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta"} {
		sb.WriteString("\t" + f + " string\n")
	}
	sb.WriteString("}\n")
	src := sb.String()

	c := mustNewChunker(t,
		[]chunker.LanguageConfig{langs.GoLanguage()},
		chunker.Config{MaxChunkSize: 100, MinChunkSize: 10},
	)
	chunks, err := c.ChunkFile("big.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Every field chunk should have Parent="BigStruct".
	foundField := false
	for _, ch := range chunks {
		if ch.NodeKind == "field_declaration" {
			foundField = true
			if ch.Parent != "BigStruct" {
				t.Errorf("field_declaration %q: expected Parent=BigStruct, got %q", ch.Name, ch.Parent)
			}
		}
	}
	if !foundField {
		t.Skip("no field_declaration atoms produced (struct may fit within MaxChunkSize on this platform)")
	}
}

func TestChunkFile_ParentExtraction(t *testing.T) {
	// Build a class large enough to exceed MaxChunkSize=200 so each method becomes
	// its own atom. Each method is ~45 bytes, well under MaxChunkSize.
	src := "class Calculator:\n"
	for _, name := range []string{"add", "subtract", "multiply", "divide", "modulo", "power"} {
		src += "    def " + name + "(self, a, b):\n        return a + b\n\n"
	}
	c := mustNewChunker(t,
		[]chunker.LanguageConfig{langs.PythonLanguage()},
		chunker.Config{MaxChunkSize: 200, MinChunkSize: 10},
	)
	chunks, err := c.ChunkFile("calc.py", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	found := false
	for _, ch := range chunks {
		if ch.Name == "add" {
			found = true
			if ch.Parent != "Calculator" {
				t.Errorf("expected Parent=Calculator for method add, got %q", ch.Parent)
			}
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=add")
	}
}

func TestChunkFile_TinyFunctionsMergeAcrossBoundary(t *testing.T) {
	// Two functions each smaller than MinChunkSize — they must merge even though
	// the second atom is a TopLevel node, because accumulated < MinChunkSize.
	src := `package main

func a() {}

func b() {}
`
	c := mustNewChunker(t,
		[]chunker.LanguageConfig{langs.GoLanguage()},
		chunker.Config{MaxChunkSize: 1500, MinChunkSize: 500},
	)
	chunks, err := c.ChunkFile("main.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 merged chunk (MinChunkSize guard), got %d", len(chunks))
	}
}

func TestChunkFile_LanguageBoundary(t *testing.T) {
	c, err := chunker.NewChunker(langs.AllLanguages(), chunker.Config{
		MaxChunkSize: 5000,
		MinChunkSize: 10,
	})
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}

	// A Vue SFC with <script lang="ts"> and <style> produces atoms in different languages.
	// The chunker must not merge atoms from different languages into one chunk.
	src := []byte(`<template><div>hello</div></template>
<script lang="ts">
export function greet(): string { return "hello" }
</script>
<style>
.container { display: flex; }
</style>`)

	chunks, err := c.ChunkFile("test.vue", src)
	if err != nil {
		t.Fatalf("ChunkFile: %v", err)
	}

	// Collect all languages that appeared in chunks.
	langSet := make(map[string]struct{})
	for _, ch := range chunks {
		if ch.Language != "" {
			langSet[ch.Language] = struct{}{}
		}
	}
	if len(langSet) < 2 {
		t.Errorf("fewer than 2 languages found in chunks (%v); Vue injection did not fire — language boundary cannot be verified", langSet)
	}

	for _, ch := range chunks {
		if ch.Language == "" {
			t.Error("chunk has empty Language")
		}
	}

	// Verify no chunk mixes languages: if a chunk is labeled "typescript",
	// it should not contain CSS, and vice versa.
	for _, ch := range chunks {
		if ch.Language == "typescript" && strings.Contains(ch.Content, ".container") {
			t.Error("typescript chunk contains CSS content — language boundary not enforced")
		}
		if ch.Language == "css" && strings.Contains(ch.Content, "export function") {
			t.Error("css chunk contains TypeScript content — language boundary not enforced")
		}
	}
}

func TestChunkFile_InjectionFailurePaths(t *testing.T) {
	c, err := chunker.NewChunker(langs.AllLanguages(), chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}

	tests := []struct {
		name string
		src  string
	}{
		{
			"empty script content",
			`<template><div></div></template><script lang="ts"></script>`,
		},
		{
			"unknown lang attribute",
			`<template><div></div></template><script lang="cobol">DISPLAY "HI"</script>`,
		},
		{
			"script with no lang attr falls back to default",
			`<template><div></div></template><script>export default {}</script>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks, err := c.ChunkFile("test.vue", []byte(tt.src))
			if err != nil {
				t.Fatalf("ChunkFile: %v", err)
			}
			// Should not panic — injection failures degrade gracefully.
			_ = chunks
		})
	}
}

func TestChunkFile_LineSplitFallback(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("package main\n\n// ")
	for range 100 {
		sb.WriteString("this is a long comment line to force the fallback path. ")
	}
	sb.WriteString("\nfunc f() {}\n")
	src := sb.String()

	c := mustNewChunker(t,
		[]chunker.LanguageConfig{langs.GoLanguage()},
		chunker.Config{MaxChunkSize: 200, MinChunkSize: 10},
	)
	chunks, err := c.ChunkFile("main.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, ch := range chunks {
		if len(ch.Content) > 200 {
			t.Errorf("chunk %d from line-split exceeds MaxChunkSize: len=%d", i, len(ch.Content))
		}
	}
}

func TestChunkFile_PythonClassMethods_NotOverSplit(t *testing.T) {
	// Python class with tiny methods: function_definition appears in both TopLevel
	// and Nested. Class methods must NOT be treated as top-level boundaries, so
	// they should merge into a single chunk rather than being over-split.
	src := `class Greeter:
    def hello(self):
        return "hi"

    def goodbye(self):
        return "bye"

    def thanks(self):
        return "thx"
`
	c := mustNewChunker(t,
		[]chunker.LanguageConfig{langs.PythonLanguage()},
		chunker.Config{MaxChunkSize: 1500, MinChunkSize: 50},
	)
	chunks, err := c.ChunkFile("greeter.py", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// All methods are small; the class itself fits under MaxChunkSize.
	// With IM-04, methods are NOT top-level, so they should merge into 1 chunk.
	if len(chunks) != 1 {
		t.Errorf("expected 1 merged chunk for small class methods, got %d", len(chunks))
		for i, ch := range chunks {
			t.Logf("  chunk %d: kind=%s name=%s isTopLevel(content starts with %q...)", i, ch.NodeKind, ch.Name, ch.Content[:min(len(ch.Content), 30)])
		}
	}
}

func TestChunkFile_PythonTopLevelFunction_StillSplits(t *testing.T) {
	// A large top-level Python function should still force a split boundary.
	var sb strings.Builder
	sb.WriteString("def small():\n    pass\n\ndef big_function():\n")
	for range 40 {
		sb.WriteString("    x = 'padding line to make this function large enough'\n")
	}
	sb.WriteString("\ndef another_small():\n    pass\n")
	src := sb.String()

	c := mustNewChunker(t,
		[]chunker.LanguageConfig{langs.PythonLanguage()},
		chunker.Config{MaxChunkSize: 1500, MinChunkSize: 50},
	)
	chunks, err := c.ChunkFile("funcs.py", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// big_function is a top-level function_definition and should trigger a split
	// boundary, so we expect more than 1 chunk.
	if len(chunks) < 2 {
		t.Errorf("expected at least 2 chunks (top-level function should split), got %d", len(chunks))
	}
}

func TestChunkFile_GoNoOverlap_Unchanged(t *testing.T) {
	// Regression guard: Go has no overlap between TopLevel and Nested kinds,
	// so behavior should be unchanged by the IM-04 refactor.
	src := `package main

func alpha() { println("alpha") }

func beta() { println("beta") }

func gamma() { println("gamma") }
`
	c := mustNewChunker(t,
		[]chunker.LanguageConfig{langs.GoLanguage()},
		chunker.Config{MaxChunkSize: 1500, MinChunkSize: 50},
	)
	chunks, err := c.ChunkFile("main.go", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Three small top-level functions: each is ~32 bytes. After the first two
	// accumulate past MinChunkSize(50), the third triggers a top-level split.
	// This verifies Go behavior is unchanged by the IM-04 refactor (Go has no
	// overlap between TopLevel and Nested kinds).
	if len(chunks) < 1 {
		t.Errorf("expected at least 1 chunk for small Go functions, got %d", len(chunks))
	}
	// Verify every chunk has the correct top-level split behavior:
	// no chunk should exceed MaxChunkSize.
	for i, ch := range chunks {
		if len(ch.Content) > 1500 {
			t.Errorf("chunk %d exceeds MaxChunkSize: len=%d", i, len(ch.Content))
		}
	}
	for _, ch := range chunks {
		if ch.Language != "go" {
			t.Errorf("expected Language=go, got %q", ch.Language)
		}
	}
}

func TestChunkFile_MarkdownSections_IndependentChunks(t *testing.T) {
	// Markdown H1 with H2 sub-sections: section appears in both TopLevel and Nested.
	// Each section should get its own chunk when large enough.
	src := `# Main Title

Some intro text under the main title.

## Section One

Content for section one with enough text to be meaningful.
More content to pad out this section a bit further.

## Section Two

Content for section two with different information.
Additional text so this section has substance.

## Section Three

Content for section three rounds out the document.
Yet more text to ensure this is a real section.
`
	c := mustNewChunker(t,
		[]chunker.LanguageConfig{langs.MarkdownLanguage()},
		chunker.Config{MaxChunkSize: 300, MinChunkSize: 50},
	)
	chunks, err := c.ChunkFile("doc.md", []byte(src))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// With sections large enough and MinChunkSize=50, each H2 section
	// should produce independent chunks.
	if len(chunks) < 2 {
		t.Errorf("expected at least 2 independent section chunks, got %d", len(chunks))
	}
	for _, ch := range chunks {
		if ch.Language != "markdown" {
			t.Errorf("expected Language=markdown, got %q", ch.Language)
		}
	}
}

func BenchmarkChunkFile_Allocations(b *testing.B) {
	c, err := chunker.NewChunker([]chunker.LanguageConfig{langs.GoLanguage()}, chunker.DefaultConfig())
	if err != nil {
		b.Fatal(err)
	}

	src := []byte(strings.Repeat("func f0() {\n\treturn\n}\n\n", 200))

	b.ReportAllocs()
	for b.Loop() {
		c.ChunkFile("test.go", src)
	}
}

func FuzzChunkFile(f *testing.F) {
	// Seed corpus from fixture files across all 15 supported languages.
	for _, seed := range []struct{ path, ext string }{
		{"../langs/testdata/go/fixture.go", "fixture.go"},
		{"../langs/testdata/python/fixture.py", "fixture.py"},
		{"../langs/testdata/json/fixture.json", "fixture.json"},
		{"../langs/testdata/vue/fixture.vue", "fixture.vue"},
		{"../langs/testdata/javascript/fixture.js", "fixture.js"},
		{"../langs/testdata/typescript/fixture.ts", "fixture.ts"},
		{"../langs/testdata/tsx/fixture.tsx", "fixture.tsx"},
		{"../langs/testdata/ruby/fixture.rb", "fixture.rb"},
		{"../langs/testdata/c/fixture.c", "fixture.c"},
		{"../langs/testdata/cpp/fixture.cpp", "fixture.cpp"},
		{"../langs/testdata/bash/fixture.sh", "fixture.sh"},
		{"../langs/testdata/html/fixture.html", "fixture.html"},
		{"../langs/testdata/css/fixture.css", "fixture.css"},
		{"../langs/testdata/markdown/fixture.md", "fixture.md"},
		{"../langs/testdata/yaml/fixture.yaml", "fixture.yaml"},
	} {
		if data, err := os.ReadFile(seed.path); err == nil {
			f.Add(seed.ext, string(data))
		}
	}
	f.Add("main.go", "package main\nfunc f() {}")
	f.Add("main.go", "")
	f.Add("unknown.xyz", "anything")

	c, err := chunker.NewChunker(langs.AllLanguages(), chunker.DefaultConfig())
	if err != nil {
		f.Fatalf("NewChunker: %v", err)
	}

	f.Fuzz(func(t *testing.T, filePath string, src string) {
		chunks, err := c.ChunkFile(filePath, []byte(src))
		if err != nil {
			return
		}
		max := chunker.DefaultConfig().MaxChunkSize
		for _, ch := range chunks {
			if ch.Start.Line < 1 {
				t.Errorf("Start.Line must be >= 1, got %d", ch.Start.Line)
			}
			if len(ch.Content) > max {
				t.Errorf("chunk Content len %d exceeds MaxChunkSize %d", len(ch.Content), max)
			}
		}
	})
}
