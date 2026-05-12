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

// Symbol is a named code symbol extracted from a chunk, used for repo-map generation.
type Symbol struct {
	FilePath  string // path to the source file containing this symbol
	Name      string // symbol name (e.g. "Store", "Search")
	NodeKind  string // tree-sitter node kind (e.g. "function_declaration")
	Parent    string // containing symbol for nested nodes; empty for top-level
	StartLine int    // 1-based start line in the source file
	EndLine   int    // 1-based end line of the symbol's span
	Language  string // language name, e.g. "go", "markdown"
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
	// ReplaceByFile atomically replaces all chunks for filePath with records,
	// and updates the file hash to fileHash, in a single transaction.
	// If records is empty, all existing chunks for filePath are removed and the
	// file hash is updated (equivalent to clearing a file's index entry).
	ReplaceByFile(ctx context.Context, filePath string, records []Record, fileHash string) error

	// Search runs hybrid KNN + BM25 search and returns results merged via
	// Reciprocal Rank Fusion, optionally filtered by language and path glob.
	Search(ctx context.Context, query SearchQuery) ([]SearchResult, error)

	// FileHash returns the stored content hash for filePath, used to detect
	// whether a file has changed since the last index run.
	FileHash(ctx context.Context, filePath string) (string, error)
	// ChunkHashesByFile returns a map of chunk ID → content hash for all
	// chunks belonging to filePath. Used to detect which chunks changed.
	ChunkHashesByFile(ctx context.Context, filePath string) (map[string]string, error)
	// EmbeddingsByContentHash returns cached embeddings for the given content
	// hashes. Chunks whose hash is present can skip the embedding API call.
	EmbeddingsByContentHash(ctx context.Context, contentHashes []string) (map[string][]float32, error)
	// ListFiles returns the absolute paths of all indexed source files.
	ListFiles(ctx context.Context) ([]string, error)
	// SetFileHash records the content hash for filePath after a successful index.
	SetFileHash(ctx context.Context, filePath, contentHash string) error
	// Stats returns aggregate chunk and file counts for the indexed codebase.
	Stats(ctx context.Context) (IndexStats, error)
	// ListSymbols returns all named symbols ordered by file path and start line.
	// Unnamed chunks (e.g. comments) are excluded.
	ListSymbols(ctx context.Context) ([]Symbol, error)

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
