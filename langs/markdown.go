// Package langs provides tree-sitter language configurations for the chunker.
//
// Markdown note: github.com/tree-sitter-grammars/tree-sitter-markdown@v0.5.3 does not
// publish a bindings/go package, so the C sources are vendored under internal/markdown_cgo
// and compiled via CGo. When a proper Go binding becomes available, replace this with
// the standard sitter.NewLanguage(markdown.Language()) pattern used by all other languages.
package langs

// #cgo CFLAGS: -std=c11 -fPIC -I${SRCDIR}/internal/markdown_cgo
// #include "internal/markdown_cgo/parser.c"
// #include "internal/markdown_cgo/scanner.c"
import "C"

import (
	"sync"
	"unsafe"

	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/ieshan/codamigo/chunker"
)

func markdownLanguagePtr() unsafe.Pointer {
	return unsafe.Pointer(C.tree_sitter_markdown())
}

var markdownLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "markdown",
		Extensions: []string{".md", ".markdown"},
		Grammar:    sitter.NewLanguage(markdownLanguagePtr()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"section", "atx_heading", "fenced_code_block"},
			Nested:    []string{},
			NameField: "heading_content",
		},
	}
})

// MarkdownLanguage returns the LanguageConfig for Markdown files.
func MarkdownLanguage() chunker.LanguageConfig {
	return markdownLanguage()
}
