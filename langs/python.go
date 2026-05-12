package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	python "github.com/tree-sitter/tree-sitter-python/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var pythonLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "python",
		Extensions: []string{".py", ".pyw"},
		Grammar:    sitter.NewLanguage(python.Language()),
		// function_definition appears in both TopLevel and Nested intentionally:
		// it represents both module-level functions (top-level boundary) and class
		// methods (nested construct). The chunker resolves the distinction via AST
		// parent-chain walk at atom collection time.
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"function_definition", "class_definition", "decorated_definition"},
			Nested:    []string{"function_definition"},
			NameField: "name",
		},
	}
})

// PythonLanguage returns the LanguageConfig for Python source files (.py, .pyw).
func PythonLanguage() chunker.LanguageConfig {
	return pythonLanguage()
}
