package langs

import (
	"sync"

	sitter "github.com/tree-sitter/go-tree-sitter"
	cpp "github.com/tree-sitter/tree-sitter-cpp/bindings/go"

	"github.com/ieshan/codamigo/chunker"
)

var cppLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "cpp",
		Extensions: []string{".cpp", ".cc", ".cxx", ".hpp"},
		Grammar:    sitter.NewLanguage(cpp.Language()),
		// function_definition appears in both TopLevel and Nested intentionally:
		// it represents both free functions (top-level boundary) and class member
		// functions (nested construct). The chunker resolves the distinction via
		// AST parent-chain walk at atom collection time.
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"function_definition", "class_specifier", "struct_specifier", "declaration"},
			Nested:    []string{"function_definition"},
			NameField: "name",
		},
	}
})

// CppLanguage returns the LanguageConfig for C++ source files (.cpp, .cc, .cxx, .hpp).
func CppLanguage() chunker.LanguageConfig {
	return cppLanguage()
}
