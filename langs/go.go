package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	golang "github.com/tree-sitter/tree-sitter-go/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var goLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "go",
		Extensions: []string{".go"},
		Grammar:    sitter.NewLanguage(golang.Language()),
		NodeKinds: chunker.NodeKindSet{
			// type_spec is included alongside type_declaration so that when an oversized
			// struct is recursed into, extractParentName lands on type_spec (which has a
			// direct "name" field) rather than type_declaration (which does not).
			TopLevel:  []string{"function_declaration", "method_declaration", "type_declaration", "type_spec", "var_declaration", "const_declaration"},
			Nested:    []string{"field_declaration"},
			NameField: "name",
		},
	}
})

// GoLanguage returns the LanguageConfig for Go source files.
func GoLanguage() chunker.LanguageConfig {
	return goLanguage()
}
