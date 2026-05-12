package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	bash "github.com/tree-sitter/tree-sitter-bash/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var bashLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "bash",
		Extensions: []string{".sh", ".bash"},
		Grammar:    sitter.NewLanguage(bash.Language()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"function_definition"},
			Nested:    []string{},
			NameField: "name",
		},
	}
})

// BashLanguage returns the LanguageConfig for Bash scripts (.sh, .bash).
func BashLanguage() chunker.LanguageConfig {
	return bashLanguage()
}
