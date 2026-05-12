package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	c "github.com/tree-sitter/tree-sitter-c/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var cLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "c",
		Extensions: []string{".c", ".h"},
		Grammar:    sitter.NewLanguage(c.Language()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"function_definition", "declaration"},
			Nested:    []string{},
			NameField: "",
		},
	}
})

// CLanguage returns the LanguageConfig for C source files (.c, .h).
// Symbol names are not extracted (NameField is empty) because C function names
// are nested inside declarator chains rather than at a direct "name" field.
func CLanguage() chunker.LanguageConfig {
	return cLanguage()
}
