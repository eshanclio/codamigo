package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	json "github.com/tree-sitter/tree-sitter-json/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var jsonLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "json",
		Extensions: []string{".json"},
		Grammar:    sitter.NewLanguage(json.Language()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"pair", "object", "array"},
			Nested:    []string{"pair"},
			NameField: "key",
		},
	}
})

// JSONLanguage returns the LanguageConfig for JSON files (.json).
func JSONLanguage() chunker.LanguageConfig {
	return jsonLanguage()
}
