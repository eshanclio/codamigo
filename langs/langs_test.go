package langs_test

import (
	"os"
	"testing"

	"github.com/ieshan/codamigo/chunker"
	"github.com/ieshan/codamigo/langs"
)

func smokeTest(t *testing.T, lang chunker.LanguageConfig, fixturePath string) []chunker.Chunk {
	t.Helper()
	src, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := chunker.NewChunker([]chunker.LanguageConfig{lang}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	chunks, err := c.ChunkFile(fixturePath, src)
	if err != nil {
		t.Fatalf("ChunkFile: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk, got 0")
	}
	for i, ch := range chunks {
		if len(ch.Content) > chunker.DefaultConfig().MaxChunkSize {
			t.Errorf("chunk %d exceeds MaxChunkSize: len=%d", i, len(ch.Content))
		}
		if ch.NodeKind == "" {
			t.Errorf("chunk %d has empty NodeKind", i)
		}
	}
	return chunks
}

func TestGoLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.GoLanguage(), "testdata/go/fixture.go")
	found := false
	for _, ch := range chunks {
		if ch.Name == "NewServer" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=NewServer")
	}
}

func TestPythonLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.PythonLanguage(), "testdata/python/fixture.py")
	found := false
	for _, ch := range chunks {
		if ch.Name == "multiply" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=multiply")
	}
}

func TestJavaScriptLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.JavaScriptLanguage(), "testdata/javascript/fixture.js")
	found := false
	for _, ch := range chunks {
		if ch.Name == "formatName" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=formatName")
	}
}

func TestTypeScriptLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.TypeScriptLanguage(), "testdata/typescript/fixture.ts")
	found := false
	for _, ch := range chunks {
		if ch.Name == "getUserById" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=getUserById")
	}
}

func TestTSXLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.TSXLanguage(), "testdata/tsx/fixture.tsx")
	found := false
	for _, ch := range chunks {
		if ch.Name == "Button" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=Button")
	}
}

func TestRubyLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.RubyLanguage(), "testdata/ruby/fixture.rb")
	found := false
	for _, ch := range chunks {
		if ch.Name == "create_animal" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=create_animal")
	}
}

func TestCLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.CLanguage(), "testdata/c/fixture.c")
	found := false
	for _, ch := range chunks {
		if ch.NodeKind == "function_definition" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one chunk with NodeKind=function_definition")
	}
}

func TestCppLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.CppLanguage(), "testdata/cpp/fixture.cpp")
	// C++ function names are inside declarator chains so Name is "" for
	// function_definition chunks. Class names are directly on class_specifier.
	found := false
	for _, ch := range chunks {
		if ch.Name == "Logger" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=Logger (class_specifier)")
	}
}

func TestBashLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.BashLanguage(), "testdata/bash/fixture.sh")
	found := false
	for _, ch := range chunks {
		if ch.Name == "greet" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected chunk with Name=greet")
	}
}

func TestHTMLLanguage(t *testing.T) {
	smokeTest(t, langs.HTMLLanguage(), "testdata/html/fixture.html")
}

func TestCSSLanguage(t *testing.T) {
	chunks := smokeTest(t, langs.CSSLanguage(), "testdata/css/fixture.css")
	// Verify that @media blocks produce a media_statement chunk (not silently dropped).
	found := false
	for _, ch := range chunks {
		if ch.NodeKind == "media_statement" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one chunk with NodeKind=media_statement for the @media rule")
	}
}

func TestMarkdownLanguage(t *testing.T) {
	smokeTest(t, langs.MarkdownLanguage(), "testdata/markdown/fixture.md")
}

func TestJSONLanguage(t *testing.T) {
	smokeTest(t, langs.JSONLanguage(), "testdata/json/fixture.json")
}

func TestYAMLLanguage(t *testing.T) {
	smokeTest(t, langs.YAMLLanguage(), "testdata/yaml/fixture.yaml")
}

func TestVueLanguage_TypeScript(t *testing.T) {
	chunks := smokeTest(t, langs.VueLanguage(), "testdata/vue/fixture.vue")

	byName := make(map[string]chunker.Chunk)
	for _, ch := range chunks {
		byName[ch.Name] = ch
	}

	t.Run("template emitted as vue chunk", func(t *testing.T) {
		found := false
		for _, ch := range chunks {
			if ch.NodeKind == "template_element" && ch.Language == "vue" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected a template_element chunk with Language=vue")
		}
	})

	t.Run("script functions emitted as typescript chunks", func(t *testing.T) {
		ch, ok := byName["validateEmail"]
		if !ok {
			t.Fatal("expected chunk with Name=validateEmail")
		}
		if ch.Language != "typescript" {
			t.Errorf("expected Language=typescript, got %q", ch.Language)
		}
		if ch.NodeKind != "function_declaration" {
			t.Errorf("expected NodeKind=function_declaration, got %q", ch.NodeKind)
		}
	})

	t.Run("line numbers are relative to original vue file", func(t *testing.T) {
		ch, ok := byName["validateEmail"]
		if !ok {
			t.Skip("validateEmail chunk not found")
		}
		// validateEmail starts on line 10 of fixture.vue (after the 7-line template
		// block, blank line, and <script lang="ts"> opening tag).
		if ch.Start.Line < 10 {
			t.Errorf("validateEmail should start at line >= 10 in the .vue file, got %d", ch.Start.Line)
		}
	})
}

func TestVueLanguage_JavaScript(t *testing.T) {
	chunks := smokeTest(t, langs.VueLanguage(), "testdata/vue/fixture-js.vue")

	byName := make(map[string]chunker.Chunk)
	for _, ch := range chunks {
		byName[ch.Name] = ch
	}

	t.Run("script function without lang attr is javascript", func(t *testing.T) {
		ch, ok := byName["greet"]
		if !ok {
			t.Fatal("expected chunk with Name=greet")
		}
		if ch.Language != "javascript" {
			t.Errorf("expected Language=javascript, got %q", ch.Language)
		}
		if ch.NodeKind != "function_declaration" {
			t.Errorf("expected NodeKind=function_declaration, got %q", ch.NodeKind)
		}
	})
}

func TestVueLanguage_ScriptSetup(t *testing.T) {
	chunks := smokeTest(t, langs.VueLanguage(), "testdata/vue/fixture-setup.vue")

	t.Run("script setup produces typescript chunks", func(t *testing.T) {
		hasTS := false
		for _, ch := range chunks {
			if ch.Language == "typescript" {
				hasTS = true
				break
			}
		}
		if !hasTS {
			t.Error("expected at least one chunk with Language=typescript from <script setup lang=\"ts\">")
		}
	})

	t.Run("no raw script_element chunk remains", func(t *testing.T) {
		for _, ch := range chunks {
			if ch.NodeKind == "script_element" {
				t.Error("script_element should be replaced by injected typescript chunks")
			}
		}
	})

	t.Run("function declarations are present", func(t *testing.T) {
		found := false
		for _, ch := range chunks {
			if ch.Language == "typescript" && (ch.NodeKind == "function_declaration" || ch.NodeKind == "lexical_declaration") {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected function_declaration or lexical_declaration chunks from script setup")
		}
	})
}

func TestVueLanguage_TemplateOnly(t *testing.T) {
	cfg := langs.VueLanguage()
	src, err := os.ReadFile("testdata/vue/fixture-template-only.vue")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	c, err := chunker.NewChunker([]chunker.LanguageConfig{cfg}, chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}
	chunks, err := c.ChunkFile("fixture-template-only.vue", src)
	if err != nil {
		t.Fatalf("ChunkFile: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one chunk")
	}

	for _, ch := range chunks {
		if ch.Language != "vue" {
			t.Errorf("expected all chunks to have Language=vue, got %q for NodeKind=%q", ch.Language, ch.NodeKind)
		}
		if ch.NodeKind == "script_element" {
			t.Error("unexpected script_element chunk in template-only file")
		}
	}

	found := false
	for _, ch := range chunks {
		if ch.NodeKind == "template_element" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a template_element chunk")
	}
}

func TestVueLanguage_StyleElement(t *testing.T) {
	chunks := smokeTest(t, langs.VueLanguage(), "testdata/vue/fixture.vue")

	found := false
	for _, ch := range chunks {
		if ch.NodeKind == "style_element" && ch.Language == "vue" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a style_element chunk with Language=vue")
	}
}

func TestAllLanguages_NoDuplicateExtensions(t *testing.T) {
	all := langs.AllLanguages()
	seen := make(map[string]string)
	for _, lang := range all {
		for _, ext := range lang.Extensions {
			if existing, ok := seen[ext]; ok {
				t.Errorf("extension %q claimed by both %q and %q", ext, existing, lang.Name)
			}
			seen[ext] = lang.Name
		}
	}
}

func TestAllLanguages_ConstructsChunker(t *testing.T) {
	c, err := chunker.NewChunker(langs.AllLanguages(), chunker.DefaultConfig())
	if err != nil {
		t.Fatalf("NewChunker with AllLanguages: %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil Chunker")
	}
}

func TestAllLanguages_Coverage(t *testing.T) {
	all := langs.AllLanguages()
	names := make(map[string]struct{}, len(all))
	for _, l := range all {
		names[l.Name] = struct{}{}
	}
	expected := []string{
		"go", "python", "javascript", "typescript", "tsx",
		"ruby", "c", "cpp", "bash", "html", "css",
		"markdown", "json", "yaml", "vue",
	}
	for _, name := range expected {
		if _, ok := names[name]; !ok {
			t.Errorf("AllLanguages() missing %q", name)
		}
	}
}

func TestMarkdownLanguage_HeadingNames(t *testing.T) {
	src := `# Installation

Install the package.

## Prerequisites

You need Go 1.26.

## Quick Start

Run the tool.
`
	c, err := chunker.NewChunker(
		[]chunker.LanguageConfig{langs.MarkdownLanguage()},
		chunker.Config{MaxChunkSize: 60, MinChunkSize: 10},
	)
	if err != nil {
		t.Fatalf("NewChunker: %v", err)
	}

	chunks, err := c.ChunkFile("README.md", []byte(src))
	if err != nil {
		t.Fatalf("ChunkFile: %v", err)
	}

	hasName := false
	for _, ch := range chunks {
		if ch.Name != "" {
			hasName = true
			break
		}
	}
	if !hasName {
		t.Error("expected at least one markdown chunk with non-empty Name from heading_content")
	}
}
