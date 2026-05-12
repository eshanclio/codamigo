package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var rubyLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "ruby",
		Extensions: []string{".rb"},
		Grammar:    sitter.NewLanguage(ruby.Language()),
		// method and singleton_method appear in both TopLevel and Nested
		// intentionally: they represent both module-level methods (top-level
		// boundary) and class methods (nested construct). The chunker resolves the
		// distinction via AST parent-chain walk at atom collection time.
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"class", "module", "method", "singleton_method"},
			Nested:    []string{"method", "singleton_method"},
			NameField: "name",
		},
	}
})

// RubyLanguage returns the LanguageConfig for Ruby source files (.rb).
func RubyLanguage() chunker.LanguageConfig {
	return rubyLanguage()
}
