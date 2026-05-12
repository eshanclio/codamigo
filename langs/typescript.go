package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var typeScriptLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "typescript",
		Extensions: []string{".ts", ".mts"},
		Grammar:    sitter.NewLanguage(typescript.LanguageTypescript()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"function_declaration", "class_declaration", "interface_declaration", "type_alias_declaration", "lexical_declaration", "export_statement"},
			Nested:    []string{"method_definition", "method_signature"},
			NameField: "name",
		},
	}
})

// TypeScriptLanguage returns the LanguageConfig for TypeScript files (.ts, .mts).
func TypeScriptLanguage() chunker.LanguageConfig {
	return typeScriptLanguage()
}
