package langs

import (
	"sync"

	yaml "github.com/tree-sitter-grammars/tree-sitter-yaml/bindings/go"
	sitter "github.com/tree-sitter/go-tree-sitter"

	"github.com/ieshan/codamigo/chunker"
)

var yamlLanguage = sync.OnceValue(func() chunker.LanguageConfig {
	return chunker.LanguageConfig{
		Name:       "yaml",
		Extensions: []string{".yaml", ".yml"},
		Grammar:    sitter.NewLanguage(yaml.Language()),
		NodeKinds: chunker.NodeKindSet{
			TopLevel:  []string{"block_mapping_pair", "block_sequence_item"},
			Nested:    []string{"block_mapping_pair"},
			NameField: "key",
		},
	}
})

// YAMLLanguage returns the LanguageConfig for YAML files (.yaml, .yml).
func YAMLLanguage() chunker.LanguageConfig {
	return yamlLanguage()
}
