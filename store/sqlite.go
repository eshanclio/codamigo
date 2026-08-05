package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3"
)

func init() {
	sqlite_vec.Auto()
}

const schemaVersion = "3"

type sqliteStore struct {
	reader       *sql.DB
	writer       *sql.DB
	embeddingDim int
}

var _ Store = (*sqliteStore)(nil)

// NewSQLiteStore opens or creates an SQLite database at dbPath with sqlite-vec and FTS5.
// On first run it creates the schema; on subsequent runs it validates that the embedding
// model and dimension match. Returns an error if they differ.
func NewSQLiteStore(dbPath string, embeddingModel string, embeddingDim int) (Store, error) {
	if embeddingDim <= 0 {
		return nil, fmt.Errorf("embeddingDim must be positive, got %d", embeddingDim)
	}

	if dbPath == ":memory:" {
		return newMemoryStore(embeddingModel, embeddingDim)
	}

	if dir := filepath.Dir(dbPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating store directory: %w", err)
		}
	}

	// PRAGMAs set via DSN so they apply to every connection in the pool.
	// _cache_size=-20000 = 20 MB page cache (negative means KB).
	// _mmap_size=268435456 = 256 MB memory-mapped I/O for reads.
	pragmas := "_journal_mode=WAL&_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-20000&_mmap_size=268435456&_temp_store=MEMORY"

	writer, err := sql.Open("sqlite3", dbPath+"?"+pragmas+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("opening writer database: %w", err)
	}
	writer.SetMaxOpenConns(1)

	if err = writer.Ping(); err != nil {
		writer.Close()
		return nil, fmt.Errorf("connecting to writer database: %w", err)
	}

	reader, err := sql.Open("sqlite3", dbPath+"?"+pragmas+"&mode=ro")
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("opening reader database: %w", err)
	}
	maxReaders := max(min(runtime.NumCPU(), 4), 2)
	reader.SetMaxOpenConns(maxReaders)

	s := &sqliteStore{reader: reader, writer: writer, embeddingDim: embeddingDim}
	if err = s.initSchema(context.Background(), embeddingModel, embeddingDim); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf("initializing schema: %w", err)
	}

	return s, nil
}

func newMemoryStore(embeddingModel string, embeddingDim int) (Store, error) {
	pragmas := "_busy_timeout=5000&_synchronous=NORMAL&_cache_size=-20000&_temp_store=MEMORY"
	db, err := sql.Open("sqlite3", ":memory:?"+pragmas+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("opening in-memory database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err = db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to in-memory database: %w", err)
	}

	s := &sqliteStore{reader: db, writer: db, embeddingDim: embeddingDim}
	if err = s.initSchema(context.Background(), embeddingModel, embeddingDim); err != nil {
		db.Close()
		return nil, fmt.Errorf("initializing schema: %w", err)
	}

	return s, nil
}

func (s *sqliteStore) initSchema(ctx context.Context, embeddingModel string, embeddingDim int) error {
	var tableCount int
	err := s.reader.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='metadata'").Scan(&tableCount)
	if err != nil {
		return fmt.Errorf("checking metadata table: %w", err)
	}

	if tableCount == 0 {
		return s.createSchema(ctx, embeddingModel, embeddingDim)
	}
	return s.validateSchema(ctx, embeddingModel, embeddingDim)
}

func (s *sqliteStore) createSchema(ctx context.Context, embeddingModel string, embeddingDim int) error {
	ddl := []string{
		`CREATE TABLE metadata (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE files (
			path         TEXT PRIMARY KEY,
			content_hash TEXT NOT NULL,
			indexed_at   INTEGER NOT NULL,
			mtime        INTEGER NOT NULL DEFAULT 0,
			size         INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE chunks (
			id           TEXT PRIMARY KEY,
			file_path    TEXT NOT NULL,
			language     TEXT NOT NULL,
			content      TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			node_kind    TEXT NOT NULL,
			name         TEXT NOT NULL DEFAULT '',
			parent       TEXT NOT NULL DEFAULT '',
			start_line   INTEGER NOT NULL,
			end_line     INTEGER NOT NULL
		)`,
		`CREATE INDEX idx_chunks_file_path ON chunks(file_path)`,
		`CREATE INDEX idx_chunks_content_hash ON chunks(content_hash)`,
		`CREATE INDEX idx_chunks_language ON chunks(language)`,
		// Resolving an edge target to a definition looks chunks up by name.
		`CREATE INDEX idx_chunks_name ON chunks(name)`,
		fmt.Sprintf(`CREATE VIRTUAL TABLE vec_chunks USING vec0(
			id TEXT PRIMARY KEY,
			language TEXT,
			embedding float[%d]
		)`, embeddingDim),
		`CREATE VIRTUAL TABLE chunks_fts USING fts5(
			id UNINDEXED,
			content,
			name,
			parent,
			tokenize='unicode61'
		)`,
		// Edges and imports are keyed by file_path as well as src_id so a
		// file's graph can be replaced wholesale alongside its chunks: chunk
		// IDs are content-derived and therefore change on every edit, which
		// makes src_id alone unusable for cleanup.
		`CREATE TABLE edges (
			src_id        TEXT NOT NULL,
			file_path     TEXT NOT NULL,
			src_name      TEXT NOT NULL DEFAULT '',
			kind          TEXT NOT NULL,
			dst_name      TEXT NOT NULL,
			dst_qualifier TEXT NOT NULL DEFAULT '',
			line          INTEGER NOT NULL
		)`,
		`CREATE INDEX idx_edges_src ON edges(src_id)`,
		`CREATE INDEX idx_edges_dst ON edges(dst_name)`,
		`CREATE INDEX idx_edges_file ON edges(file_path)`,
		`CREATE TABLE file_imports (
			file_path TEXT NOT NULL,
			module    TEXT NOT NULL,
			alias     TEXT NOT NULL DEFAULT '',
			line      INTEGER NOT NULL
		)`,
		`CREATE INDEX idx_file_imports_path ON file_imports(file_path)`,
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	for _, stmt := range ddl {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("executing DDL: %w", err)
		}
	}

	meta := map[string]string{
		"schema_version":  schemaVersion,
		"embedding_model": embeddingModel,
		"embedding_dim":   strconv.Itoa(embeddingDim),
	}
	for k, v := range meta {
		if _, err := tx.ExecContext(ctx, "INSERT INTO metadata (key, value) VALUES (?, ?)", k, v); err != nil {
			return fmt.Errorf("inserting metadata %q: %w", k, err)
		}
	}

	return tx.Commit()
}

func (s *sqliteStore) validateSchema(ctx context.Context, embeddingModel string, embeddingDim int) error {
	var storedModel, storedDim, storedVersion string

	row := s.reader.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = 'schema_version'")
	if err := row.Scan(&storedVersion); err != nil {
		return fmt.Errorf("reading schema_version: %w", err)
	}

	stored, err := strconv.Atoi(storedVersion)
	if err != nil {
		return fmt.Errorf("parsing schema_version %q: %w", storedVersion, err)
	}
	current, _ := strconv.Atoi(schemaVersion) // schemaVersion is a package constant
	if stored != current {
		return fmt.Errorf("database schema version %s does not match current version %s; re-index required — run 'codamigo reset' then 'codamigo index'", storedVersion, schemaVersion)
	}

	row = s.reader.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = 'embedding_model'")
	if err = row.Scan(&storedModel); err != nil {
		return fmt.Errorf("reading embedding_model: %w", err)
	}

	row = s.reader.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = 'embedding_dim'")
	if err = row.Scan(&storedDim); err != nil {
		return fmt.Errorf("reading embedding_dim: %w", err)
	}

	if storedModel != embeddingModel {
		return fmt.Errorf("embedding model changed from %q to %q; re-index required — run 'codamigo reset' then 'codamigo index'", storedModel, embeddingModel)
	}

	dim, err := strconv.Atoi(storedDim)
	if err != nil {
		return fmt.Errorf("parsing embedding_dim %q: %w", storedDim, err)
	}
	if dim != embeddingDim {
		return fmt.Errorf("embedding dimension changed from %d to %d; re-index required — run 'codamigo reset' then 'codamigo index'", dim, embeddingDim)
	}

	return nil
}

func (s *sqliteStore) Meta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.reader.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading metadata %q: %w", key, err)
	}
	return value, nil
}

func (s *sqliteStore) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.writer.ExecContext(ctx, "INSERT OR REPLACE INTO metadata (key, value) VALUES (?, ?)", key, value)
	if err != nil {
		return fmt.Errorf("writing metadata %q: %w", key, err)
	}
	return nil
}

func (s *sqliteStore) Close() error {
	s.writer.ExecContext(context.Background(), "PRAGMA optimize") //nolint:errcheck
	if s.reader != s.writer {
		s.reader.ExecContext(context.Background(), "PRAGMA optimize") //nolint:errcheck
	}
	var errs []error
	if err := s.writer.Close(); err != nil {
		errs = append(errs, err)
	}
	if s.reader != s.writer {
		if err := s.reader.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *sqliteStore) Checkpoint(ctx context.Context) error {
	_, err := s.writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	if err != nil {
		return fmt.Errorf("wal checkpoint: %w", err)
	}
	return nil
}

// quoteFTSTokens wraps each space-separated token in double quotes to prevent
// FTS5 query syntax interpretation (e.g. AND, OR, NOT, NEAR).
func quoteFTSTokens(s string) string {
	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	for i, tok := range tokens {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('"')
		b.WriteString(strings.ReplaceAll(tok, `"`, `""`))
		b.WriteByte('"')
	}
	return b.String()
}

func serializeFloat32(v []float32) ([]byte, error) {
	if len(v) == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	return sqlite_vec.SerializeFloat32(v)
}

func (s *sqliteStore) Upsert(ctx context.Context, records []Record) error {
	if len(records) == 0 {
		return nil
	}

	for _, r := range records {
		if len(r.Embedding) != s.embeddingDim {
			return fmt.Errorf("record %q: embedding length %d does not match store dimension %d",
				r.ID, len(r.Embedding), s.embeddingDim)
		}
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	chunkStmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO chunks (id, file_path, language, content, content_hash, node_kind, name, parent, start_line, end_line)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing chunks insert: %w", err)
	}
	defer chunkStmt.Close()

	vecDeleteStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM vec_chunks WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("preparing vec delete: %w", err)
	}
	defer vecDeleteStmt.Close()

	vecStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO vec_chunks (id, language, embedding) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing vec insert: %w", err)
	}
	defer vecStmt.Close()

	ftsDeleteStmt, err := tx.PrepareContext(ctx,
		`DELETE FROM chunks_fts WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("preparing fts delete: %w", err)
	}
	defer ftsDeleteStmt.Close()

	ftsInsertStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO chunks_fts (id, content, name, parent) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing fts insert: %w", err)
	}
	defer ftsInsertStmt.Close()

	for _, r := range records {
		if _, err = chunkStmt.ExecContext(ctx, r.ID, r.FilePath, r.Language, r.Content, r.ContentHash, r.NodeKind, r.Name, r.Parent, r.StartLine, r.EndLine); err != nil {
			return fmt.Errorf("inserting chunk %q: %w", r.ID, err)
		}

		if _, err = vecDeleteStmt.ExecContext(ctx, r.ID); err != nil {
			return fmt.Errorf("deleting vec %q: %w", r.ID, err)
		}

		vecBlob, err := serializeFloat32(r.Embedding)
		if err != nil {
			return fmt.Errorf("serializing embedding for %q: %w", r.ID, err)
		}
		if _, err = vecStmt.ExecContext(ctx, r.ID, r.Language, vecBlob); err != nil {
			return fmt.Errorf("inserting vec %q: %w", r.ID, err)
		}

		if _, err = ftsDeleteStmt.ExecContext(ctx, r.ID); err != nil {
			return fmt.Errorf("deleting fts %q: %w", r.ID, err)
		}

		tokenContent := TokenizeForSearch(r.Content)
		tokenName := TokenizeForSearch(r.Name)
		tokenParent := TokenizeForSearch(r.Parent)
		if _, err = ftsInsertStmt.ExecContext(ctx, r.ID, tokenContent, tokenName, tokenParent); err != nil {
			return fmt.Errorf("inserting fts %q: %w", r.ID, err)
		}
	}

	return tx.Commit()
}

func (s *sqliteStore) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	delChunk, err := tx.PrepareContext(ctx, "DELETE FROM chunks WHERE id = ?")
	if err != nil {
		return fmt.Errorf("preparing chunk delete: %w", err)
	}
	defer delChunk.Close()

	delVec, err := tx.PrepareContext(ctx, "DELETE FROM vec_chunks WHERE id = ?")
	if err != nil {
		return fmt.Errorf("preparing vec delete: %w", err)
	}
	defer delVec.Close()

	delFts, err := tx.PrepareContext(ctx, "DELETE FROM chunks_fts WHERE id = ?")
	if err != nil {
		return fmt.Errorf("preparing fts delete: %w", err)
	}
	defer delFts.Close()

	for _, id := range ids {
		if _, err = delChunk.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("deleting chunk %q: %w", id, err)
		}
		if _, err = delVec.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("deleting vec %q: %w", id, err)
		}
		if _, err = delFts.ExecContext(ctx, id); err != nil {
			return fmt.Errorf("deleting fts %q: %w", id, err)
		}
	}

	return tx.Commit()
}

func (s *sqliteStore) DeleteByFile(ctx context.Context, filePath string) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, "SELECT id FROM chunks WHERE file_path = ?", filePath)
	if err != nil {
		return fmt.Errorf("querying chunks for file %q: %w", filePath, err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return fmt.Errorf("scanning chunk id: %w", err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iterating chunk ids: %w", err)
	}
	rows.Close()

	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "DELETE FROM vec_chunks WHERE id = ?", id); err != nil {
			return fmt.Errorf("deleting vec %q: %w", id, err)
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM chunks_fts WHERE id = ?", id); err != nil {
			return fmt.Errorf("deleting fts %q: %w", id, err)
		}
	}

	if _, err = tx.ExecContext(ctx, "DELETE FROM chunks WHERE file_path = ?", filePath); err != nil {
		return fmt.Errorf("deleting chunks for file %q: %w", filePath, err)
	}

	if err = deleteGraphForFile(ctx, tx, filePath); err != nil {
		return err
	}

	if _, err = tx.ExecContext(ctx, "DELETE FROM files WHERE path = ?", filePath); err != nil {
		return fmt.Errorf("deleting file record %q: %w", filePath, err)
	}

	return tx.Commit()
}

// deleteGraphForFile removes a file's edges and imports. Both are file-scoped,
// so they are replaced wholesale rather than diffed per chunk.
func deleteGraphForFile(ctx context.Context, tx *sql.Tx, filePath string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM edges WHERE file_path = ?", filePath); err != nil {
		return fmt.Errorf("deleting edges for file %q: %w", filePath, err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM file_imports WHERE file_path = ?", filePath); err != nil {
		return fmt.Errorf("deleting imports for file %q: %w", filePath, err)
	}
	return nil
}

func (s *sqliteStore) ReplaceByFiles(ctx context.Context, entries []FileRecords) error {
	if len(entries) == 0 {
		return nil
	}

	for _, entry := range entries {
		for _, r := range entry.Records {
			if len(r.Embedding) != s.embeddingDim {
				return fmt.Errorf("record %q: embedding length %d does not match store dimension %d",
					r.ID, len(r.Embedding), s.embeddingDim)
			}
		}
	}

	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

	chunkStmt, err := tx.PrepareContext(ctx,
		`INSERT OR REPLACE INTO chunks (id, file_path, language, content, content_hash, node_kind, name, parent, start_line, end_line)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing chunks insert: %w", err)
	}
	defer chunkStmt.Close()

	vecDeleteStmt, err := tx.PrepareContext(ctx, `DELETE FROM vec_chunks WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("preparing vec delete: %w", err)
	}
	defer vecDeleteStmt.Close()

	vecStmt, err := tx.PrepareContext(ctx, `INSERT INTO vec_chunks (id, language, embedding) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing vec insert: %w", err)
	}
	defer vecStmt.Close()

	ftsDeleteStmt, err := tx.PrepareContext(ctx, `DELETE FROM chunks_fts WHERE id = ?`)
	if err != nil {
		return fmt.Errorf("preparing fts delete: %w", err)
	}
	defer ftsDeleteStmt.Close()

	ftsStmt, err := tx.PrepareContext(ctx, `INSERT INTO chunks_fts (id, content, name, parent) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing fts insert: %w", err)
	}
	defer ftsStmt.Close()

	edgeStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO edges (src_id, file_path, src_name, kind, dst_name, dst_qualifier, line) VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing edges insert: %w", err)
	}
	defer edgeStmt.Close()

	importStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO file_imports (file_path, module, alias, line) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("preparing imports insert: %w", err)
	}
	defer importStmt.Close()

	for _, entry := range entries {
		rows, err := tx.QueryContext(ctx, "SELECT id FROM chunks WHERE file_path = ?", entry.FilePath)
		if err != nil {
			return fmt.Errorf("querying chunks for file %q: %w", entry.FilePath, err)
		}
		var oldIDs []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("scanning chunk id: %w", err)
			}
			oldIDs = append(oldIDs, id)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterating chunk ids: %w", err)
		}
		rows.Close()

		for _, id := range oldIDs {
			if _, err = vecDeleteStmt.ExecContext(ctx, id); err != nil {
				return fmt.Errorf("deleting vec %q: %w", id, err)
			}
			if _, err = ftsDeleteStmt.ExecContext(ctx, id); err != nil {
				return fmt.Errorf("deleting fts %q: %w", id, err)
			}
		}

		if _, err = tx.ExecContext(ctx, "DELETE FROM chunks WHERE file_path = ?", entry.FilePath); err != nil {
			return fmt.Errorf("deleting chunks for file %q: %w", entry.FilePath, err)
		}

		if err = deleteGraphForFile(ctx, tx, entry.FilePath); err != nil {
			return err
		}

		for _, r := range entry.Records {
			if _, err = chunkStmt.ExecContext(ctx, r.ID, r.FilePath, r.Language, r.Content, r.ContentHash, r.NodeKind, r.Name, r.Parent, r.StartLine, r.EndLine); err != nil {
				return fmt.Errorf("inserting chunk %q: %w", r.ID, err)
			}
			if _, err = vecDeleteStmt.ExecContext(ctx, r.ID); err != nil {
				return fmt.Errorf("deleting vec %q: %w", r.ID, err)
			}
			vecBlob, err := serializeFloat32(r.Embedding)
			if err != nil {
				return fmt.Errorf("serializing embedding for %q: %w", r.ID, err)
			}
			if _, err = vecStmt.ExecContext(ctx, r.ID, r.Language, vecBlob); err != nil {
				return fmt.Errorf("inserting vec %q: %w", r.ID, err)
			}
			if _, err = ftsDeleteStmt.ExecContext(ctx, r.ID); err != nil {
				return fmt.Errorf("deleting fts %q: %w", r.ID, err)
			}
			tokenContent := TokenizeForSearch(r.Content)
			tokenName := TokenizeForSearch(r.Name)
			tokenParent := TokenizeForSearch(r.Parent)
			if _, err = ftsStmt.ExecContext(ctx, r.ID, tokenContent, tokenName, tokenParent); err != nil {
				return fmt.Errorf("inserting fts %q: %w", r.ID, err)
			}
		}

		for _, e := range entry.Edges {
			if _, err = edgeStmt.ExecContext(ctx, e.SrcID, entry.FilePath, e.SrcName, e.Kind, e.DstName, e.DstQualifier, e.Line); err != nil {
				return fmt.Errorf("inserting edge %s->%s for %q: %w", e.SrcID, e.DstName, entry.FilePath, err)
			}
		}

		for _, im := range entry.Imports {
			if _, err = importStmt.ExecContext(ctx, entry.FilePath, im.Module, im.Alias, im.Line); err != nil {
				return fmt.Errorf("inserting import %q for %q: %w", im.Module, entry.FilePath, err)
			}
		}

		if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO files (path, content_hash, indexed_at, mtime, size) VALUES (?, ?, ?, ?, ?)",
			entry.FilePath, entry.FileHash, time.Now().Unix(), entry.Mtime, entry.Size); err != nil {
			return fmt.Errorf("setting file hash for %q: %w", entry.FilePath, err)
		}
	}

	return tx.Commit()
}

func (s *sqliteStore) Search(ctx context.Context, q SearchQuery) ([]SearchResult, error) {
	if q.Limit <= 0 {
		return []SearchResult{}, nil
	}

	hasFilters := len(q.Languages) > 0 || len(q.Paths) > 0 || len(q.Names) > 0 || len(q.NodeKinds) > 0

	const maxIterations = 3
	fetchLimit := max((q.Limit+q.Offset)*3, 1)

	for iteration := range maxIterations {
		results, err := s.searchOnce(ctx, q, fetchLimit)
		if err != nil {
			return nil, err
		}
		if len(results) >= q.Limit || !hasFilters || iteration == maxIterations-1 {
			return results, nil
		}
		fetchLimit *= 3
	}

	return []SearchResult{}, nil
}

func (s *sqliteStore) searchOnce(ctx context.Context, q SearchQuery, fetchLimit int) ([]SearchResult, error) {
	type scored struct {
		id       string
		vecRank  int
		bm25Rank int
	}

	idScores := make(map[string]*scored)

	// Step 1: KNN vector search with optional single-language pre-filter.
	if len(q.Embedding) > 0 {
		vecBlob, err := serializeFloat32(q.Embedding)
		if err != nil {
			return nil, fmt.Errorf("serializing query embedding: %w", err)
		}

		var vecQuery string
		var vecArgs []any
		if len(q.Languages) == 1 {
			vecQuery = "SELECT id, distance FROM vec_chunks WHERE embedding MATCH ? AND k = ? AND language = ?"
			vecArgs = []any{vecBlob, fetchLimit, q.Languages[0]}
		} else {
			vecQuery = "SELECT id, distance FROM vec_chunks WHERE embedding MATCH ? AND k = ?"
			vecArgs = []any{vecBlob, fetchLimit}
		}

		vecRows, err := s.reader.QueryContext(ctx, vecQuery, vecArgs...)
		if err != nil {
			return nil, fmt.Errorf("vector search: %w", err)
		}

		rank := 1
		for vecRows.Next() {
			var id string
			var dist float64
			if err = vecRows.Scan(&id, &dist); err != nil {
				vecRows.Close()
				return nil, fmt.Errorf("scanning vector result: %w", err)
			}
			idScores[id] = &scored{id: id, vecRank: rank}
			rank++
		}
		if err = vecRows.Err(); err != nil {
			vecRows.Close()
			return nil, fmt.Errorf("iterating vector results: %w", err)
		}
		vecRows.Close()
	}

	// Step 2: BM25 full-text search with optional filter JOIN.
	if q.Text != "" {
		ftsQuery := quoteFTSTokens(TokenizeForSearch(q.Text))
		if ftsQuery != "" {
			query, args := s.buildFTSQuery(ftsQuery, fetchLimit, q)
			ftsRows, err := s.reader.QueryContext(ctx, query, args...)
			if err != nil {
				return nil, fmt.Errorf("bm25 search: %w", err)
			}
			defer ftsRows.Close()

			rank := 1
			for ftsRows.Next() {
				var id string
				var bm25Rank float64
				if err = ftsRows.Scan(&id, &bm25Rank); err != nil {
					return nil, fmt.Errorf("scanning bm25 result: %w", err)
				}
				if sc, ok := idScores[id]; ok {
					sc.bm25Rank = rank
				} else {
					idScores[id] = &scored{id: id, bm25Rank: rank}
				}
				rank++
			}
			if err = ftsRows.Err(); err != nil {
				return nil, fmt.Errorf("iterating bm25 results: %w", err)
			}
		}
	}

	if len(idScores) == 0 {
		return []SearchResult{}, nil
	}

	// Step 3: Reciprocal Rank Fusion (k=60).
	const rrfK = 60.0
	type idScore struct {
		id    string
		score float64
	}

	ranked := make([]idScore, 0, len(idScores))
	for _, sc := range idScores {
		score := 0.0
		if sc.vecRank > 0 {
			score += 1.0 / (rrfK + float64(sc.vecRank))
		}
		if sc.bm25Rank > 0 {
			score += 1.0 / (rrfK + float64(sc.bm25Rank))
		}
		ranked = append(ranked, idScore{id: sc.id, score: score})
	}

	slices.SortFunc(ranked, func(a, b idScore) int {
		return cmp.Compare(b.score, a.score)
	})

	// Step 4: SQL batch fetch with filters.
	ids := make([]string, len(ranked))
	for i, r := range ranked {
		ids[i] = r.id
	}

	recordMap, err := s.fetchRecordsFiltered(ctx, ids, q)
	if err != nil {
		return nil, fmt.Errorf("fetching records: %w", err)
	}

	// Step 5: Apply path glob filter in Go and assemble results.
	results := make([]SearchResult, 0, q.Limit)
	skipped := 0
	for _, r := range ranked {
		if len(results) >= q.Limit {
			break
		}
		rec, ok := recordMap[r.id]
		if !ok {
			continue
		}

		if !matchesPathFilter(rec.FilePath, q.Paths) {
			continue
		}

		if skipped < q.Offset {
			skipped++
			continue
		}

		results = append(results, SearchResult{
			Record: *rec,
			Score:  float32(r.score),
		})
	}

	return results, nil
}

func (s *sqliteStore) buildFTSQuery(ftsQuery string, fetchLimit int, q SearchQuery) (string, []any) {
	needsJoin := len(q.Languages) > 0 || len(q.Names) > 0 || len(q.NodeKinds) > 0
	if !needsJoin {
		return "SELECT id, rank FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY rank LIMIT ?",
			[]any{ftsQuery, fetchLimit}
	}

	var b strings.Builder
	args := []any{ftsQuery}

	b.WriteString("SELECT f.id, f.rank FROM chunks_fts f JOIN chunks c ON c.id = f.id WHERE f.chunks_fts MATCH ?")

	if len(q.Languages) > 0 {
		b.WriteString(" AND c.language IN (")
		b.WriteString(placeholders(len(q.Languages)))
		b.WriteString(")")
		for _, l := range q.Languages {
			args = append(args, l)
		}
	}
	if len(q.Names) > 0 {
		b.WriteString(" AND c.name IN (")
		b.WriteString(placeholders(len(q.Names)))
		b.WriteString(")")
		for _, n := range q.Names {
			args = append(args, n)
		}
	}
	if len(q.NodeKinds) > 0 {
		b.WriteString(" AND c.node_kind IN (")
		b.WriteString(placeholders(len(q.NodeKinds)))
		b.WriteString(")")
		for _, nk := range q.NodeKinds {
			args = append(args, nk)
		}
	}

	b.WriteString(" ORDER BY f.rank LIMIT ?")
	args = append(args, fetchLimit)

	return b.String(), args
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n)[:n*2-1]
}

func (s *sqliteStore) fetchRecordsFiltered(ctx context.Context, ids []string, q SearchQuery) (map[string]*Record, error) {
	if len(ids) == 0 {
		return map[string]*Record{}, nil
	}

	result := make(map[string]*Record, len(ids))

	const chunkSize = 500
	for start := 0; start < len(ids); start += chunkSize {
		end := min(start+chunkSize, len(ids))
		batch := ids[start:end]

		var b strings.Builder
		if q.MetadataOnly {
			b.WriteString("SELECT id, file_path, language, node_kind, name, parent, start_line, end_line FROM chunks WHERE id IN (")
		} else {
			b.WriteString("SELECT id, file_path, language, content, content_hash, node_kind, name, parent, start_line, end_line FROM chunks WHERE id IN (")
		}
		b.WriteString(placeholders(len(batch)))
		b.WriteString(")")

		args := make([]any, 0, len(batch)+len(q.Languages)+len(q.Names)+len(q.NodeKinds))
		for _, id := range batch {
			args = append(args, id)
		}

		if len(q.Languages) > 0 {
			b.WriteString(" AND language IN (")
			b.WriteString(placeholders(len(q.Languages)))
			b.WriteString(")")
			for _, l := range q.Languages {
				args = append(args, l)
			}
		}
		if len(q.Names) > 0 {
			b.WriteString(" AND name IN (")
			b.WriteString(placeholders(len(q.Names)))
			b.WriteString(")")
			for _, n := range q.Names {
				args = append(args, n)
			}
		}
		if len(q.NodeKinds) > 0 {
			b.WriteString(" AND node_kind IN (")
			b.WriteString(placeholders(len(q.NodeKinds)))
			b.WriteString(")")
			for _, nk := range q.NodeKinds {
				args = append(args, nk)
			}
		}

		rows, err := s.reader.QueryContext(ctx, b.String(), args...)
		if err != nil {
			return nil, fmt.Errorf("batch fetching records: %w", err)
		}

		for rows.Next() {
			var r Record
			if q.MetadataOnly {
				if err = rows.Scan(&r.ID, &r.FilePath, &r.Language, &r.NodeKind, &r.Name, &r.Parent, &r.StartLine, &r.EndLine); err != nil {
					rows.Close()
					return nil, fmt.Errorf("scanning record: %w", err)
				}
			} else {
				if err = rows.Scan(&r.ID, &r.FilePath, &r.Language, &r.Content, &r.ContentHash,
					&r.NodeKind, &r.Name, &r.Parent, &r.StartLine, &r.EndLine); err != nil {
					rows.Close()
					return nil, fmt.Errorf("scanning record: %w", err)
				}
			}
			result[r.ID] = &r
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterating records: %w", err)
		}
		rows.Close()
	}

	return result, nil
}

func (s *sqliteStore) FileHashes(ctx context.Context, filePaths []string) (map[string]string, error) {
	if len(filePaths) == 0 {
		return make(map[string]string), nil
	}

	result := make(map[string]string, len(filePaths))

	const batchSize = 500
	for start := 0; start < len(filePaths); start += batchSize {
		end := min(start+batchSize, len(filePaths))
		batch := filePaths[start:end]

		ph := placeholders(len(batch))
		args := make([]any, len(batch))
		for i, p := range batch {
			args[i] = p
		}

		rows, err := s.reader.QueryContext(ctx,
			"SELECT path, content_hash FROM files WHERE path IN ("+ph+")", args...)
		if err != nil {
			return nil, fmt.Errorf("querying file hashes: %w", err)
		}

		for rows.Next() {
			var path, hash string
			if err = rows.Scan(&path, &hash); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scanning file hash: %w", err)
			}
			result[path] = hash
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterating file hashes: %w", err)
		}
		rows.Close()
	}

	return result, nil
}

func (s *sqliteStore) FileStates(ctx context.Context, filePaths []string) (map[string]FileState, error) {
	if len(filePaths) == 0 {
		return make(map[string]FileState), nil
	}

	result := make(map[string]FileState, len(filePaths))

	const batchSize = 500
	for start := 0; start < len(filePaths); start += batchSize {
		end := min(start+batchSize, len(filePaths))
		batch := filePaths[start:end]

		ph := placeholders(len(batch))
		args := make([]any, len(batch))
		for i, p := range batch {
			args[i] = p
		}

		rows, err := s.reader.QueryContext(ctx,
			"SELECT path, content_hash, mtime, size FROM files WHERE path IN ("+ph+")", args...)
		if err != nil {
			return nil, fmt.Errorf("querying file states: %w", err)
		}

		for rows.Next() {
			var path string
			var st FileState
			if err = rows.Scan(&path, &st.ContentHash, &st.Mtime, &st.Size); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scanning file state: %w", err)
			}
			result[path] = st
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterating file states: %w", err)
		}
		rows.Close()
	}

	return result, nil
}

func (s *sqliteStore) ChunkHashesByFile(ctx context.Context, filePath string) (map[string]string, error) {
	rows, err := s.reader.QueryContext(ctx, "SELECT id, content_hash FROM chunks WHERE file_path = ?", filePath)
	if err != nil {
		return nil, fmt.Errorf("querying chunks for file %q: %w", filePath, err)
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var id, hash string
		if err = rows.Scan(&id, &hash); err != nil {
			return nil, fmt.Errorf("scanning chunk hash: %w", err)
		}
		result[id] = hash
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating chunk hashes for file %q: %w", filePath, err)
	}
	return result, nil
}

func (s *sqliteStore) EmbeddingsByContentHash(ctx context.Context, contentHashes []string) (map[string][]float32, error) {
	if len(contentHashes) == 0 {
		return nil, nil
	}

	result := make(map[string][]float32, len(contentHashes))

	const batchSize = 500
	for start := 0; start < len(contentHashes); start += batchSize {
		end := min(start+batchSize, len(contentHashes))
		batch := contentHashes[start:end]

		if err := func() error {
			ph := placeholders(len(batch))

			args := make([]any, len(batch))
			for i, ch := range batch {
				args[i] = ch
			}

			var qb strings.Builder
			qb.WriteString(`SELECT c.content_hash, v.embedding FROM chunks c
			 JOIN vec_chunks v ON c.id = v.id
			 WHERE c.id IN (
			   SELECT MIN(c2.id) FROM chunks c2 WHERE c2.content_hash IN (`)
			qb.WriteString(ph)
			qb.WriteString(`
			   ) GROUP BY c2.content_hash
			 )`)

			rows, err := s.reader.QueryContext(ctx, qb.String(), args...)
			if err != nil {
				return fmt.Errorf("querying embeddings by content hash: %w", err)
			}
			defer rows.Close()

			for rows.Next() {
				var ch string
				var blob []byte
				if err = rows.Scan(&ch, &blob); err != nil {
					return fmt.Errorf("scanning embedding: %w", err)
				}
				emb, err := deserializeFloat32(blob, s.embeddingDim)
				if err != nil {
					return fmt.Errorf("deserializing embedding: %w", err)
				}
				result[ch] = emb
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}

	return result, nil
}

// matchesPathFilter reports whether filePath matches any of the glob patterns.
// Returns true when patterns is empty (no filtering). Supports filepath.Match
// syntax plus a special case for "dir/**" (prefix match). Arbitrary ** in the
// middle of a pattern (e.g. "src/**/test_*.go") is not supported and will not
// match; use multiple simple patterns instead.
func matchesPathFilter(filePath string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		matched, err := filepath.Match(pattern, filePath)
		if err == nil && matched {
			return true
		}
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "/**")
			if strings.HasPrefix(filePath, prefix+"/") {
				return true
			}
		}
	}
	return false
}

func deserializeFloat32(buf []byte, dim int) ([]float32, error) {
	if len(buf) != dim*4 {
		return nil, fmt.Errorf("expected %d bytes for %d-dim vector, got %d", dim*4, dim, len(buf))
	}
	result := make([]float32, dim)
	for i := range dim {
		bits := uint32(buf[i*4]) | uint32(buf[i*4+1])<<8 | uint32(buf[i*4+2])<<16 | uint32(buf[i*4+3])<<24
		result[i] = math.Float32frombits(bits)
	}
	return result, nil
}

func (s *sqliteStore) ListFiles(ctx context.Context) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, "SELECT path FROM files")
	if err != nil {
		return nil, fmt.Errorf("listing files: %w", err)
	}
	defer rows.Close()

	paths := make([]string, 0)
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scanning file path: %w", err)
		}
		paths = append(paths, p)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating file paths: %w", err)
	}
	return paths, nil
}

func (s *sqliteStore) Stats(ctx context.Context) (IndexStats, error) {
	var stats IndexStats

	if err := s.reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM chunks").Scan(&stats.ChunkCount); err != nil {
		return IndexStats{}, fmt.Errorf("counting chunks: %w", err)
	}
	if err := s.reader.QueryRowContext(ctx, "SELECT COUNT(*) FROM files").Scan(&stats.FileCount); err != nil {
		return IndexStats{}, fmt.Errorf("counting files: %w", err)
	}

	rows, err := s.reader.QueryContext(ctx, "SELECT language, COUNT(*) FROM chunks GROUP BY language")
	if err != nil {
		return IndexStats{}, fmt.Errorf("querying language stats: %w", err)
	}
	defer rows.Close()

	stats.Languages = make(map[string]int)
	for rows.Next() {
		var lang string
		var count int
		if err = rows.Scan(&lang, &count); err != nil {
			return IndexStats{}, fmt.Errorf("scanning language stat: %w", err)
		}
		stats.Languages[lang] = count
	}
	if err = rows.Err(); err != nil {
		return IndexStats{}, fmt.Errorf("iterating language stats: %w", err)
	}

	return stats, nil
}

func (s *sqliteStore) ListSymbols(ctx context.Context) ([]Symbol, error) {
	var count int
	if err := s.reader.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM chunks WHERE name != ''").Scan(&count); err != nil {
		return nil, fmt.Errorf("counting symbols: %w", err)
	}
	if count == 0 {
		return nil, nil
	}

	rows, err := s.reader.QueryContext(ctx,
		"SELECT id, file_path, name, node_kind, parent, start_line, end_line, language FROM chunks WHERE name != '' ORDER BY file_path, start_line")
	if err != nil {
		return nil, fmt.Errorf("listing symbols: %w", err)
	}
	defer rows.Close()

	symbols := make([]Symbol, 0, count)
	for rows.Next() {
		var sym Symbol
		if err = rows.Scan(&sym.ID, &sym.FilePath, &sym.Name, &sym.NodeKind, &sym.Parent, &sym.StartLine, &sym.EndLine, &sym.Language); err != nil {
			return nil, fmt.Errorf("scanning symbol: %w", err)
		}
		symbols = append(symbols, sym)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating symbols: %w", err)
	}
	return symbols, nil
}
