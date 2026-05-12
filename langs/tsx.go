package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var tsxLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "tsx",
		Extensions: []string{".tsx"},
		Grammar:    sitter.NewLanguage(typescript.LanguageTSX()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"function_declaration", "class_declaration", "interface_declaration", "type_alias_declaration", "lexical_declaration", "export_statement"},
			Nested:    []string{"method_definition", "method_signature"},
			NameField: "name",
		},
	}
})

// TSXLanguage returns the LanguageConfig for TSX files (.tsx).
func TSXLanguage() chunker.LanguageConfig {
	return tsxLanguage()
}
