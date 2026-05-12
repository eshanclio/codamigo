package chunker

// Position is a point in source text.
type Position struct {
	Line       int // 1-based
	Column     int // 0-based
	ByteOffset int
}

// Chunk is one semantically coherent unit of source code, ready for embedding.
type Chunk struct {
	Content  string   // raw source text of this chunk
	FilePath string   // absolute path to the source file
	Language string   // language name as reported by LanguageConfig.Name
	NodeKind string   // tree-sitter node kind, e.g. "function_declaration", "class_definition"
	Name     string   // symbol name from the NameField, e.g. "MyFunc"; empty if not extractable
	Parent   string   // containing symbol for nested nodes (e.g. class name for a method); empty for top-level
	Start    Position // start of the chunk in the source file (inclusive)
	End      Position // end of the chunk in the source file (inclusive)
}
