// Package store owns the persistence layer for codamigo: the [Record] and
// [SearchQuery] types, the [Store] interface, and a sqlite-vec implementation.
//
// [Store] is the single interface callers use. The sqlite-vec implementation
// maintains four tables: chunks (metadata), vec_chunks (KNN vectors),
// chunks_fts (FTS5 keyword index), and files (file-level hash tracking).
// Hybrid search merges KNN and BM25 results using Reciprocal Rank Fusion.
// store never imports chunker; the field mapping is performed in indexer.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// Record is the unit of storage for a single code chunk with its embedding vector.
type Record struct {
	ID          string    // 64-char hex SHA-256 derived from FilePath + Content
	FilePath    string    // absolute path to the source file
	Language    string    // language name, e.g. "go"
	Content     string    // raw source text of the chunk
	ContentHash string    // SHA-256 of Content; used for embedding reuse across files
	NodeKind    string    // tree-sitter node kind, e.g. "function_declaration"
	Name        string    // symbol name; empty when not extractable
	Parent      string    // containing symbol for nested nodes; empty for top-level
	StartLine   int       // 1-based start line in the source file
	EndLine     int       // 1-based end line in the source file
	Embedding   []float32 // float32 vector produced by the embedding model
}

// SearchQuery specifies parameters for a hybrid vector + keyword search.
type SearchQuery struct {
	Embedding    []float32 // query embedding for KNN; required
	Text         string    // query text for BM25; also used in Reciprocal Rank Fusion
	Limit        int       // maximum number of results to return
	Offset       int       // number of results to skip after ranking (pagination)
	Languages    []string  // optional language filter; empty means all languages
	Paths        []string  // glob patterns for file path filtering ("dir/**" supported; arbitrary ** mid-pattern is not)
	Names        []string  // optional symbol name filter (exact match via SQL IN); empty means all names
	NodeKinds    []string  // optional node kind filter (exact match via SQL IN, e.g. "function_declaration"); empty means all
	MetadataOnly bool      // when true, omit Content and ContentHash from results
}

// SearchResult is a Record augmented with a relevance score from hybrid search.
type SearchResult struct {
	Record
	Score float32
}

// IndexStats holds aggregate counts for the indexed codebase.
type IndexStats struct {
	ChunkCount int            // total number of chunks across all indexed files
	FileCount  int            // number of distinct source files in the index
	Languages  map[string]int // chunk count per language name
}

// Symbol is a named code symbol extracted from a chunk, used for repo-map
// generation and for resolving edge targets to definitions.
type Symbol struct {
	ID        string // chunk ID this symbol was extracted from
	FilePath  string // path to the source file containing this symbol
	Name      string // symbol name (e.g. "Store", "Search")
	NodeKind  string // tree-sitter node kind (e.g. "function_declaration")
	Parent    string // containing symbol for nested nodes; empty for top-level
	StartLine int    // 1-based start line in the source file
	EndLine   int    // 1-based end line of the symbol's span
	Language  string // language name, e.g. "go", "markdown"
}

// Edge is a relationship from an indexed chunk to a named target, extracted
// from the AST at index time.
//
// DstName is deliberately unresolved: it is the identifier as written in the
// source, because mapping it to a definition needs whole-project knowledge.
// Resolution happens at query time; see the query package.
type Edge struct {
	SrcID    string // chunks.id of the chunk containing the reference
	FilePath string // path to the source file, for file-scoped replacement
	// SrcName is the enclosing definition's name as reported by the parser.
	// It is kept alongside SrcID because a large function may be split across
	// several chunks, leaving the chunk that holds the reference unnamed; the
	// parser still knows which definition the reference sits in.
	SrcName      string // enclosing definition name; empty when the parser could not determine one
	Kind         string // "call", "inherit", or "reference"
	DstName      string // referenced identifier as written
	DstQualifier string // receiver or package part when qualified (e.g. "fmt" in fmt.Println)
	Line         int    // 1-based line of the reference
}

// Import is a module imported by a source file. Imports are file-scoped rather
// than symbol-to-symbol, so they are tracked separately from Edge and are used
// to disambiguate which file an edge target resolves to.
type Import struct {
	FilePath string // path to the importing source file
	Module   string // module path as written, e.g. "fmt", "./mod", "stdio.h"
	Alias    string // local alias when the import declares one; empty otherwise
	Line     int    // 1-based line of the import
}

// FileRecords groups records for a single file with its content hash,
// used for batched write operations.
type FileRecords struct {
	FilePath string
	Records  []Record
	FileHash string
	// Mtime is the file's on-disk modification time (Unix seconds) at index
	// time. Zero when unknown. Used with Size as a cheap staleness fast-path.
	Mtime int64
	// Size is the file's on-disk size in bytes at index time. Zero when unknown.
	Size int64
	// Edges are the graph edges extracted from this file. Replaced wholesale
	// with the file's chunks in the same transaction.
	Edges []Edge
	// Imports are the modules this file imports. Replaced wholesale with the
	// file's chunks in the same transaction.
	Imports []Import
}

// FileState is the persisted per-file state recorded at index time, used for
// query-time staleness detection: the (Mtime, Size) pair is a cheap change
// signal, and ContentHash is the authoritative one checked only when the pair
// differs from disk.
type FileState struct {
	ContentHash string
	Mtime       int64
	Size        int64
}

// Store is the persistence interface for code chunk CRUD and search operations.
type Store interface {
	// Upsert writes records to the chunk, vector, and FTS tables in a single
	// transaction, inserting or replacing on ID conflict.
	Upsert(ctx context.Context, records []Record) error
	// Delete removes records with the given IDs from all content tables.
	Delete(ctx context.Context, ids []string) error
	// DeleteByFile removes all chunks for filePath from all content tables.
	DeleteByFile(ctx context.Context, filePath string) error
	// FileHashes returns a map of filePath → contentHash for all given paths.
	// Paths not in the store are absent from the returned map.
	FileHashes(ctx context.Context, filePaths []string) (map[string]string, error)
	// FileStates returns a map of filePath → FileState (hash, mtime, size)
	// recorded at index time, for all given paths. Paths not in the store are
	// absent from the returned map.
	FileStates(ctx context.Context, filePaths []string) (map[string]FileState, error)
	// ReplaceByFiles atomically replaces chunks for multiple files in a single
	// transaction. Each entry's records replace all existing chunks for that file,
	// and the file hash is updated. Rolls back entirely on error.
	ReplaceByFiles(ctx context.Context, entries []FileRecords) error

	// Search runs hybrid KNN + BM25 search and returns results merged via
	// Reciprocal Rank Fusion, optionally filtered by language and path glob.
	Search(ctx context.Context, query SearchQuery) ([]SearchResult, error)

	// ChunkHashesByFile returns a map of chunk ID → content hash for all
	// chunks belonging to filePath. Used to detect which chunks changed.
	ChunkHashesByFile(ctx context.Context, filePath string) (map[string]string, error)
	// EmbeddingsByContentHash returns cached embeddings for the given content
	// hashes. Chunks whose hash is present can skip the embedding API call.
	EmbeddingsByContentHash(ctx context.Context, contentHashes []string) (map[string][]float32, error)
	// ListFiles returns the absolute paths of all indexed source files.
	ListFiles(ctx context.Context) ([]string, error)
	// Stats returns aggregate chunk and file counts for the indexed codebase.
	Stats(ctx context.Context) (IndexStats, error)
	// ListSymbols returns all named symbols ordered by file path and start line.
	// Unnamed chunks (e.g. comments) are excluded.
	ListSymbols(ctx context.Context) ([]Symbol, error)

	// EdgesBySource returns all edges originating in the given chunk IDs.
	// Used to answer "what does this symbol reference?".
	EdgesBySource(ctx context.Context, srcIDs []string) ([]Edge, error)
	// EdgesByTargetName returns all edges whose unresolved target name matches
	// name exactly. Used to answer "what references this symbol?"; callers must
	// still resolve each edge to confirm it targets the intended definition.
	EdgesByTargetName(ctx context.Context, name string) ([]Edge, error)
	// ImportsByFile returns the imports declared by each of the given files.
	// Paths with no imports are absent from the result.
	ImportsByFile(ctx context.Context, filePaths []string) ([]Import, error)
	// EdgeCount returns the total number of stored edges. Zero while chunks
	// exist means the graph has not been built yet, i.e. indexing ran with the
	// graph disabled.
	EdgeCount(ctx context.Context) (int, error)

	// Meta reads a value from the key-value metadata table.
	Meta(ctx context.Context, key string) (string, error)
	// SetMeta writes a key-value pair to the metadata table.
	SetMeta(ctx context.Context, key, value string) error

	// Close releases the database connection. Always call via defer after Open.
	Close() error
	// Checkpoint triggers a WAL checkpoint to prevent unbounded WAL growth.
	// Should be called after large batch indexing operations.
	Checkpoint(ctx context.Context) error
}

// RecordID returns a 64-char hex SHA-256 ID for a chunk.
// NUL (\x00) is used as separator rather than ":" to prevent hash collisions
// when file paths contain colons (valid on Linux/macOS).
func RecordID(filePath, content string) string {
	h := sha256.Sum256([]byte(filePath + "\x00" + content))
	return hex.EncodeToString(h[:])
}

// ContentHash returns a 64-char hex SHA-256 hash of the given content.
// Used for embedding reuse: identical content produces the same hash regardless of file location.
func ContentHash(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}
