// Vendored grammar note: github.com/tree-sitter-grammars/tree-sitter-vue does not
// publish a bindings/go package, so the C sources are vendored under internal/vue_cgo
// and compiled via CGo at commit ce8011a414fdf8091f4e4071752efc376f4afb08.
// When a proper Go binding becomes available, replace this with the standard
// sitter.NewLanguage(vue.Language()) pattern.
package langs

// #cgo CFLAGS: -std=c11 -fPIC -I${SRCDIR}/internal/vue_cgo
// #include "internal/vue_cgo/parser.c"
// #include "internal/vue_cgo/scanner.c"
import "C"

import (
	"slices"
	"sync"
	"unsafe"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/ieshan/codamigo/chunker"
)

// vueLanguagePtr returns the C pointer to the tree-sitter-vue grammar.
func vueLanguagePtr() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_vue())
}

// withoutExportStatement returns a copy of cfg with export_statement removed
// from TopLevel so the chunker descends into exported declarations and produces
// function_declaration / class_declaration chunks with resolvable names.
func withoutExportStatement(cfg chunker.LanguageConfig) chunker.LanguageConfig {
	cfg.NodeKinds.TopLevel = slices.DeleteFunc(
		slices.Clone(cfg.NodeKinds.TopLevel),
		func(k string) bool { return k == "export_statement" },
	)
	return cfg
}

var vueLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	ts := withoutExportStatement(TypeScriptLanguage())
	js := withoutExportStatement(JavaScriptLanguage())
	return chunker.LanguageConfig{
		Name:       "vue",
		Extensions: []string{".vue"},
		Grammar:    sitter.NewLanguage(vueLanguagePtr()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"template_element", "script_element", "style_element"},
			Nested:    []string{},
			NameField: "",
		},
		Injections: []chunker.InjectionRule{{
			ContainerKind: "script_element",
			ContentKind:   "raw_text",
			LangAttr:      "lang",
			DefaultLang:   "javascript",
			Grammars: map[string]chunker.LanguageConfig{
				"javascript": js,
				"js":         js,
				"typescript": ts,
				"ts":         ts,
			},
		}},
	}
})

// VueLanguage returns the LanguageConfig for Vue single-file components (.vue).
// Script blocks are re-parsed with the TypeScript or JavaScript grammar based on
// the lang attribute (e.g. <script lang="ts">), producing function-level chunks
// suitable for vector embedding RAG. <script setup> blocks are handled identically
// to plain <script> blocks — both produce function/declaration-level chunks.
func VueLanguage() chunker.LanguageConfig {
	return vueLanguage()
}
