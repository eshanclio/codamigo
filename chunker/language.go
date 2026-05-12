package chunker

import sitter "github.com/tree-sitter/go-tree-sitter"

// NodeKindSet describes which AST node types are semantic boundaries
// and how to extract symbol names from them.
type NodeKindSet struct {
	TopLevel  []string // node types that become top-level chunk boundaries
	Nested    []string // node types nested inside top-level nodes (e.g. methods in a class)
	NameField string   // tree-sitter field name for the symbol identifier, e.g. "name"; empty means skip name extraction
}

// InjectionRule instructs the Chunker to re-parse a container node's embedded
// content using a secondary grammar, replacing the section-level atom with
// semantically richer atoms from the injected parse.
type InjectionRule struct {
	ContainerKind string                    // node kind wrapping embedded content, e.g. "script_element"
	ContentKind   string                    // node kind of the raw content child, e.g. "raw_text"
	LangAttr      string                    // start_tag attribute that selects the grammar, e.g. "lang"
	DefaultLang   string                    // grammar key when LangAttr is absent, e.g. "javascript"
	Grammars      map[string]LanguageConfig // lang attribute value -> LanguageConfig
}

// LanguageConfig fully describes one language the Chunker can handle.
// Construct these in the langs/ package and pass them to NewChunker.
type LanguageConfig struct {
	Name       string           // canonical language name, e.g. "go", "python"
	Extensions []string         // file extensions including dot, e.g. [".go"], [".py", ".pyw"]
	Grammar    *sitter.Language // tree-sitter grammar; obtain via sitter.NewLanguage(binding.Language())
	NodeKinds  NodeKindSet
	Injections []InjectionRule // optional: re-parse embedded content with secondary grammars; nil means no injection
}
