// Package indexer orchestrates the walk → chunk → embed → store pipeline.
//
// [Indexer] is constructed with four injected dependencies — a chunker, an
// embedder, a store, and a walker — and knows nothing about the concrete
// implementations behind those interfaces. indexer never imports langs; the
// caller builds the *chunker.Chunker with the desired language configs and
// passes it in, keeping CGo out of the library layer.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/codamigo/walker"
	"github.com/ieshan/go-code-chunker/chunker"
	"github.com/ieshan/go-embedder"
)

// Progress receives per-file notifications during indexing.
// Implementations must be safe for concurrent use — stages run inside
// errgroup goroutines.
type Progress interface {
	// FileStarted is called when a file begins processing in the read+hash stage.
	FileStarted(path string)
	// FileProcessed is called after a file has been successfully embedded and stored.
	FileProcessed(path string)
	// FileSkipped is called when a file is bypassed due to a hash match (content
	// unchanged), an unsupported language extension, or exceeding maxFileSize.
	FileSkipped(path string)
	// FileFailed is called when a file could not be indexed because at least
	// one of its chunks failed embedding. The error is the joined per-chunk
	// failure cause. The file's existing store record (if any) is left
	// untouched so it will be retried on the next index run.
	FileFailed(path string, err error)
}

// Indexer orchestrates the walk → chunk → embed → store pipeline.
type Indexer struct {
	chunker        *chunker.Chunker
	embedder       embedder.Embedder
	store          store.Store
	walker         *walker.Walker
	fsys           fs.FS
	concurrency    int
	maxFileSize    int64
	writeBatchSize int
	onIndexed      func()
	progress       Progress
	enableGraph    bool
}

// Option customises an Indexer at construction time.
//
// Everything with a sensible default is an Option; only the dependencies an
// Indexer cannot work without are positional parameters of [New]. That keeps
// adding a knob from breaking every existing caller, and keeps call sites
// self-describing instead of a run of bare ints and nils.
type Option func(*Indexer)

// WithGraph enables or disables graph edge extraction. Enabled by default:
// edges come from the parse chunking already performs, so extracting them costs
// only an extra AST walk. Disable it to stop writing the edge tables entirely.
func WithGraph(enabled bool) Option {
	return func(idx *Indexer) { idx.enableGraph = enabled }
}

// WithConcurrency sets the maximum number of files processed in parallel.
// Non-positive values leave the default of 1 in effect.
func WithConcurrency(n int) Option {
	return func(idx *Indexer) {
		if n > 0 {
			idx.concurrency = n
		}
	}
}

// WithMaxFileSize skips files larger than n bytes. Non-positive values leave the
// default in effect, which is no limit.
func WithMaxFileSize(n int64) Option {
	return func(idx *Indexer) {
		if n > 0 {
			idx.maxFileSize = n
		}
	}
}

// WithWriteBatchSize sets how many FileRecords entries are written per store
// call. Non-positive values leave the default of 50 in effect.
func WithWriteBatchSize(n int) Option {
	return func(idx *Indexer) {
		if n > 0 {
			idx.writeBatchSize = n
		}
	}
}

// WithOnIndexed registers a callback invoked after each successful write batch,
// used to invalidate caches derived from the index.
func WithOnIndexed(fn func()) Option {
	return func(idx *Indexer) { idx.onIndexed = fn }
}

// WithProgress registers a receiver for per-file notifications. Without it,
// indexing reports no progress.
func WithProgress(p Progress) Option {
	return func(idx *Indexer) { idx.progress = p }
}

// New constructs an Indexer from its required dependencies; everything else is
// set with an [Option].
//
// c may be nil for hash-only indexing (no chunking); when non-nil it must have
// been built with the desired language configs (from langs.AllLanguages() in cmd/).
// indexer does not import langs directly.
// The caller retains ownership of w and must close it separately; the indexer
// does not close the walker.
// Graph edge extraction is on by default; pass WithGraph(false) to disable it.
func New(c *chunker.Chunker, e embedder.Embedder, s store.Store, w *walker.Walker, opts ...Option) *Indexer {
	if e == nil {
		panic("indexer.New: embedder must not be nil")
	}
	if s == nil {
		panic("indexer.New: store must not be nil")
	}
	if w == nil {
		panic("indexer.New: walker must not be nil")
	}
	idx := &Indexer{
		chunker:        c,
		embedder:       e,
		store:          s,
		walker:         w,
		fsys:           w.FS(),
		concurrency:    1,
		writeBatchSize: 50,
		enableGraph:    true,
	}
	for _, opt := range opts {
		opt(idx)
	}
	return idx
}

// Index walks the project root, chunks every matched file, embeds each chunk,
// and upserts the resulting records into the store. Files whose content hash
// matches the stored hash are skipped. Records for files that no longer exist
// on disk are removed from the store. Returns nil on context cancellation
// (partial progress is left in the store and is valid).
func (idx *Indexer) Index(ctx context.Context) error {
	var paths []string
	for path, err := range idx.walker.Walk(ctx) {
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return fmt.Errorf("walking: %w", err)
		}
		paths = append(paths, path)
	}

	if err := idx.indexBatch(ctx, paths); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	walked := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		walked[p] = struct{}{}
	}
	indexed, err := idx.store.ListFiles(ctx)
	if err != nil {
		return fmt.Errorf("listing indexed files: %w", err)
	}
	for _, path := range indexed {
		if _, ok := walked[path]; !ok {
			if err = idx.store.DeleteByFile(ctx, path); err != nil {
				return fmt.Errorf("deleting removed file %s: %w", path, err)
			}
		}
	}

	if err = idx.store.Checkpoint(ctx); err != nil {
		slog.ErrorContext(ctx, "wal checkpoint failed", slog.Any("error", err))
	}

	if idx.onIndexed != nil {
		idx.onIndexed()
	}

	return nil
}

// IndexFiles indexes a specific set of paths. Files that no longer exist on
// disk are removed from the store. Files excluded by the walker's filter are
// silently skipped.
func (idx *Indexer) IndexFiles(ctx context.Context, paths []string) error {
	var toIndex []string
	for _, path := range paths {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		relPath, relErr := filepath.Rel(idx.walker.Root(), path)
		outsideRoot := relErr != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator))

		if outsideRoot {
			if err := idx.store.DeleteByFile(ctx, path); err != nil {
				slog.ErrorContext(ctx, "deleting out-of-root file failed", slog.String("path", path), slog.Any("error", err))
			}
			continue
		}

		_, err := fs.Stat(idx.fsys, relPath)
		if errors.Is(err, fs.ErrNotExist) {
			if err = idx.store.DeleteByFile(ctx, path); err != nil {
				slog.ErrorContext(ctx, "deleting missing file failed", slog.String("path", path), slog.Any("error", err))
			}
			continue
		}
		if err != nil {
			slog.ErrorContext(ctx, "stat failed", slog.String("path", path), slog.Any("error", err))
			continue
		}

		if !idx.walker.Match(path) {
			continue
		}

		toIndex = append(toIndex, path)
	}

	if len(toIndex) > 0 {
		if err := idx.indexBatch(ctx, toIndex); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}

	if idx.onIndexed != nil {
		idx.onIndexed()
	}
	return nil
}

// StaleFiles returns the subset of the given paths whose on-disk content has
// changed since indexing, using a two-level check for query-time staleness
// detection:
//
//   - Fast path: if a file's current (mtime, size) matches its stored state,
//     it is treated as unchanged without reading the file.
//   - Confirm: otherwise the file is read and its content hash compared against
//     the stored hash, so a mere touch (mtime bump, same bytes) is not counted
//     as stale.
//
// A file missing or unreadable on disk is reported stale (its stored chunks
// should be pruned on re-index). A file with no stored state is confirmed by
// hash against an empty stored hash, so it reads as stale. Files exceeding
// maxFileSize are treated as unchanged (they are never indexed). It never
// chunks or embeds.
func (idx *Indexer) StaleFiles(ctx context.Context, paths []string, stored map[string]store.FileState) (map[string]struct{}, error) {
	stale := make(map[string]struct{})
	for _, path := range paths {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		relPath, err := filepath.Rel(idx.walker.Root(), path)
		if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
			continue
		}

		info, err := fs.Stat(idx.fsys, relPath)
		if err != nil {
			// Missing or unreadable on disk: the indexed copy is stale.
			stale[path] = struct{}{}
			continue
		}

		st, ok := stored[path]
		if ok && st.Mtime == info.ModTime().Unix() && st.Size == info.Size() {
			// Fast path: unchanged (mtime + size match) — no read needed.
			continue
		}

		content, err := fs.ReadFile(idx.fsys, relPath)
		if err != nil {
			stale[path] = struct{}{}
			continue
		}
		if idx.maxFileSize > 0 && int64(len(content)) > idx.maxFileSize {
			continue
		}

		if st.ContentHash != store.ContentHash(content) {
			stale[path] = struct{}{}
		}
	}
	return stale, nil
}

func (idx *Indexer) indexBatch(ctx context.Context, paths []string) error {
	stageBatchSize := idx.concurrency * 4
	for batch := range slices.Chunk(paths, stageBatchSize) {
		if ctx.Err() != nil {
			return nil
		}
		if err := idx.processStageBatch(ctx, batch); err != nil {
			return err
		}
	}
	return nil
}

type fileInfo struct {
	path    string
	relPath string
	content []byte
	hash    string
	mtime   int64
	size    int64
}

type embedOrigin struct {
	fileIdx  int
	chunkIdx int
}

func (idx *Indexer) processStageBatch(ctx context.Context, paths []string) error {
	// Stage 1: Read + Hash
	infos := make([]fileInfo, len(paths))
	var g errgroup.Group
	g.SetLimit(idx.concurrency)

	for i, path := range paths {
		g.Go(func() error {
			if idx.progress != nil {
				idx.progress.FileStarted(path)
			}

			relPath, err := filepath.Rel(idx.walker.Root(), path)
			if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
				slog.WarnContext(ctx, "skipping file outside project root", slog.String("path", path))
				return nil
			}

			info, err := fs.Stat(idx.fsys, relPath)
			if err != nil {
				return fmt.Errorf("stat file %s: %w", path, err)
			}

			content, err := fs.ReadFile(idx.fsys, relPath)
			if err != nil {
				return fmt.Errorf("reading file %s: %w", path, err)
			}

			if idx.maxFileSize > 0 && int64(len(content)) > idx.maxFileSize {
				slog.DebugContext(ctx, "skipping large file",
					slog.String("path", path),
					slog.Int("size", len(content)),
					slog.Int64("limit", idx.maxFileSize))
				if idx.progress != nil {
					idx.progress.FileSkipped(path)
				}
				return nil
			}

			infos[i] = fileInfo{
				path:    path,
				relPath: relPath,
				content: content,
				hash:    store.ContentHash(content),
				mtime:   info.ModTime().Unix(),
				size:    info.Size(),
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	// Collect all non-empty paths for batch hash lookup.
	hashPaths := make([]string, 0, len(infos))
	for i := range infos {
		if infos[i].path != "" {
			hashPaths = append(hashPaths, infos[i].path)
		}
	}

	existingHashes, err := idx.store.FileHashes(ctx, hashPaths)
	if err != nil {
		return fmt.Errorf("batch file hash lookup: %w", err)
	}

	// Filter to changed files only.
	var changed []fileInfo
	for i := range infos {
		if infos[i].path == "" {
			continue
		}
		if existingHashes[infos[i].path] == infos[i].hash {
			if idx.progress != nil {
				idx.progress.FileSkipped(infos[i].path)
			}
			continue
		}
		changed = append(changed, infos[i])
	}

	if len(changed) == 0 {
		return nil
	}

	// Stage 2: Chunk
	type chunkedFile struct {
		info         fileInfo
		chunks       []chunker.Chunk
		edges        []chunker.Edge // graph edges from the same parse; nil when graph is disabled
		skipProgress bool           // true when FileSkipped was already reported (e.g. unsupported language)
	}
	chunked := make([]chunkedFile, len(changed))

	var g2 errgroup.Group
	g2.SetLimit(idx.concurrency)

	for i, fi := range changed {
		g2.Go(func() error {
			if idx.chunker == nil {
				chunked[i] = chunkedFile{info: fi}
				return nil
			}
			// Analyze returns chunks and edges from one parse, so the graph
			// costs an extra AST walk rather than a second parse.
			res, err := idx.chunker.Analyze(fi.path, fi.content)
			if errors.Is(err, chunker.ErrUnsupportedLanguage) {
				slog.DebugContext(ctx, "skipping unsupported file type", slog.String("path", fi.path))
				if idx.progress != nil {
					idx.progress.FileSkipped(fi.path)
				}
				chunked[i] = chunkedFile{info: fi, skipProgress: true}
				return nil
			}
			if err != nil {
				return fmt.Errorf("chunking %s: %w", fi.path, err)
			}
			edges := res.Edges
			if !idx.enableGraph {
				edges = nil
			}
			chunked[i] = chunkedFile{info: fi, chunks: res.Chunks, edges: edges}
			return nil
		})
	}
	if err = g2.Wait(); err != nil {
		return err
	}

	// Release file content — no longer needed after chunking.
	for i := range chunked {
		chunked[i].info.content = nil
	}

	// Stage 3: Embed
	// Collect all content hashes across all files.
	var allContentHashes []string
	origins := make([]embedOrigin, 0, len(chunked))
	contentHashMap := make([][]string, len(chunked))

	for i, cf := range chunked {
		hashes := make([]string, len(cf.chunks))
		for j, ch := range cf.chunks {
			hashes[j] = store.ContentHash([]byte(ch.Content))
			allContentHashes = append(allContentHashes, hashes[j])
		}
		contentHashMap[i] = hashes
	}

	// Batch cache lookup.
	cachedEmbeddings, err := idx.store.EmbeddingsByContentHash(ctx, allContentHashes)
	if err != nil {
		return fmt.Errorf("batch embedding cache lookup: %w", err)
	}

	// Collect uncached texts with origin tracking.
	var uncachedTexts []string
	for i, cf := range chunked {
		hashes := contentHashMap[i]
		for j := range cf.chunks {
			if _, ok := cachedEmbeddings[hashes[j]]; !ok {
				origins = append(origins, embedOrigin{fileIdx: i, chunkIdx: j})
				uncachedTexts = append(uncachedTexts, cf.chunks[j].Content)
			}
		}
	}

	// Per-chunk failure isolation: failed chunks do not block successful ones.
	// We call EmbedBatchPartial which returns parallel (vectors, errs) slices
	// where errs[i] != nil indicates chunk i could not be embedded. Any file
	// owning at least one failed chunk is marked failed and skipped from
	// writing; its existing store record (if any) is left untouched so the
	// next indexing run will retry the file.
	failedFileIdx := make(map[int]struct{})
	failedFileErrs := make(map[int][]error)
	embeddingsByFile := make(map[int]map[int][]float32) // fileIdx -> chunkIdx -> embedding
	if len(uncachedTexts) > 0 {
		newEmbeddings, embedErrs := idx.embedder.EmbedBatchPartial(ctx, uncachedTexts)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if len(newEmbeddings) != len(uncachedTexts) || len(embedErrs) != len(uncachedTexts) {
			return fmt.Errorf("embedder returned %d vectors / %d errs for %d texts",
				len(newEmbeddings), len(embedErrs), len(uncachedTexts))
		}
		// Distribute results (failures and SUCCESSFUL embeddings) back to
		// per-file records in a single pass.
		for k, e := range embedErrs {
			origin := origins[k]
			if e != nil {
				failedFileIdx[origin.fileIdx] = struct{}{}
				failedFileErrs[origin.fileIdx] = append(failedFileErrs[origin.fileIdx], e)
				slog.WarnContext(ctx, "embedding chunk failed",
					slog.String("path", chunked[origin.fileIdx].info.path),
					slog.Int("chunk", origin.chunkIdx),
					slog.Any("error", e))
				continue
			}
			if embeddingsByFile[origin.fileIdx] == nil {
				embeddingsByFile[origin.fileIdx] = make(map[int][]float32)
			}
			embeddingsByFile[origin.fileIdx][origin.chunkIdx] = newEmbeddings[k]
		}
	}

	// Stage 4: Write
	// Build FileRecords entries — include all files (even unsupported-language
	// ones with empty chunks) so their hash is persisted for skip detection.
	progressReported := make(map[string]struct{})
	for _, cf := range chunked {
		if cf.skipProgress {
			progressReported[cf.info.path] = struct{}{}
		}
	}

	entries := make([]store.FileRecords, 0, len(chunked))
	for i, cf := range chunked {
		if _, failed := failedFileIdx[i]; failed {
			if idx.progress != nil {
				idx.progress.FileFailed(cf.info.path, errors.Join(failedFileErrs[i]...))
			}
			continue
		}
		hashes := contentHashMap[i]
		records := make([]store.Record, len(cf.chunks))

		for j, ch := range cf.chunks {
			var emb []float32
			if cached, ok := cachedEmbeddings[hashes[j]]; ok {
				emb = cached
			} else if fileEmbs, ok := embeddingsByFile[i]; ok {
				emb = fileEmbs[j]
			}

			records[j] = store.Record{
				ID:          store.RecordID(cf.info.path, ch.Content),
				FilePath:    cf.info.path,
				Language:    ch.Language,
				Content:     ch.Content,
				ContentHash: hashes[j],
				NodeKind:    ch.NodeKind,
				Name:        ch.Name,
				Parent:      ch.Parent,
				StartLine:   ch.Start.Line,
				EndLine:     ch.End.Line,
				Embedding:   emb,
			}
		}

		edges, imports := mapEdges(cf.info.path, cf.edges, records)

		entries = append(entries, store.FileRecords{
			FilePath: cf.info.path,
			Records:  records,
			FileHash: cf.info.hash,
			Mtime:    cf.info.mtime,
			Size:     cf.info.size,
			Edges:    edges,
			Imports:  imports,
		})
	}

	// Write in writeBatchSize groups.
	for writeBatch := range slices.Chunk(entries, idx.writeBatchSize) {
		if err = idx.store.ReplaceByFiles(ctx, writeBatch); err != nil {
			return fmt.Errorf("batch write: %w", err)
		}
		for _, entry := range writeBatch {
			if idx.progress != nil {
				if _, already := progressReported[entry.FilePath]; !already {
					idx.progress.FileProcessed(entry.FilePath)
				}
			}
		}
	}

	return nil
}
