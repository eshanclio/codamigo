package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	javascript "github.com/tree-sitter/tree-sitter-javascript/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var javaScriptLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "javascript",
		Extensions: []string{".js", ".mjs", ".cjs", ".jsx"},
		Grammar:    sitter.NewLanguage(javascript.Language()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"function_declaration", "class_declaration", "lexical_declaration", "variable_declaration", "export_statement"},
			Nested:    []string{"method_definition"},
			NameField: "name",
		},
	}
})

// JavaScriptLanguage returns the LanguageConfig for JavaScript files (.js, .mjs, .cjs, .jsx).
func JavaScriptLanguage() chunker.LanguageConfig {
	return javaScriptLanguage()
}
