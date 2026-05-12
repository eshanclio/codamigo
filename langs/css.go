package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	css "github.com/tree-sitter/tree-sitter-css/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var cssLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "css",
		Extensions: []string{".css"},
		Grammar:    sitter.NewLanguage(css.Language()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"rule_set", "at_rule", "media_statement"},
			Nested:    []string{},
			NameField: "",
		},
	}
})

// CSSLanguage returns the LanguageConfig for CSS stylesheets (.css).
func CSSLanguage() chunker.LanguageConfig {
	return cssLanguage()
}
