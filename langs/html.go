package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	html "github.com/tree-sitter/tree-sitter-html/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var htmlLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "html",
		Extensions: []string{".html", ".htm"},
		Grammar:    sitter.NewLanguage(html.Language()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"element"},
			Nested:    []string{"element"},
			NameField: "tag_name",
		},
	}
})

// HTMLLanguage returns the LanguageConfig for HTML files (.html, .htm).
func HTMLLanguage() chunker.LanguageConfig {
	return htmlLanguage()
}
