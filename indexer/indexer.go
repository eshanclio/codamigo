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

	"github.com/ieshan/codamigo/embedder"
	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/codamigo/walker"
	"github.com/ieshan/go-code-chunker/chunker"
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
}

// New constructs an Indexer from its dependencies.
// c may be nil for hash-only indexing (no chunking); when non-nil it must have
// been built with the desired language configs (from langs.AllLanguages() in cmd/).
// indexer does not import langs directly.
// The caller retains ownership of w and must close it separately; the indexer
// does not close the walker.
// concurrency controls the maximum number of files processed in parallel;
// values <= 0 are treated as 1.
// writeBatchSize controls how many FileRecords entries are written per store
// call; values <= 0 default to 50.
// progress is optional; pass nil to disable per-file notifications.
func New(c *chunker.Chunker, e embedder.Embedder, s store.Store, w *walker.Walker, concurrency int, maxFileSize int64, writeBatchSize int, onIndexed func(), progress Progress) *Indexer {
	if e == nil {
		panic("indexer.New: embedder must not be nil")
	}
	if s == nil {
		panic("indexer.New: store must not be nil")
	}
	if w == nil {
		panic("indexer.New: walker must not be nil")
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	if writeBatchSize <= 0 {
		writeBatchSize = 50
	}
	return &Indexer{
		chunker:        c,
		embedder:       e,
		store:          s,
		walker:         w,
		fsys:           w.FS(),
		concurrency:    concurrency,
		maxFileSize:    maxFileSize,
		writeBatchSize: writeBatchSize,
		onIndexed:      onIndexed,
		progress:       progress,
	}
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
		skipProgress bool // true when FileSkipped was already reported (e.g. unsupported language)
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
			chunks, err := idx.chunker.ChunkFile(fi.path, fi.content)
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
			chunked[i] = chunkedFile{info: fi, chunks: chunks}
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
	var origins []embedOrigin
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
	var newEmbeddings [][]float32
	var embedErrs []error
	failedFileIdx := make(map[int]struct{})
	failedFileErrs := make(map[int][]error)
	if len(uncachedTexts) > 0 {
		newEmbeddings, embedErrs = idx.embedder.EmbedBatchPartial(ctx, uncachedTexts)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if len(newEmbeddings) != len(uncachedTexts) || len(embedErrs) != len(uncachedTexts) {
			return fmt.Errorf("embedder returned %d vectors / %d errs for %d texts",
				len(newEmbeddings), len(embedErrs), len(uncachedTexts))
		}
		for k, e := range embedErrs {
			if e == nil {
				continue
			}
			fi := origins[k].fileIdx
			failedFileIdx[fi] = struct{}{}
			failedFileErrs[fi] = append(failedFileErrs[fi], e)
			slog.WarnContext(ctx, "embedding chunk failed",
				slog.String("path", chunked[fi].info.path),
				slog.Int("chunk", origins[k].chunkIdx),
				slog.Any("error", e))
		}
	}

	// Distribute SUCCESSFUL embeddings back to per-file records.
	embeddingsByFile := make(map[int]map[int][]float32) // fileIdx -> chunkIdx -> embedding
	for k, origin := range origins {
		if embedErrs[k] != nil {
			continue
		}
		if embeddingsByFile[origin.fileIdx] == nil {
			embeddingsByFile[origin.fileIdx] = make(map[int][]float32)
		}
		embeddingsByFile[origin.fileIdx][origin.chunkIdx] = newEmbeddings[k]
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

		entries = append(entries, store.FileRecords{
			FilePath: cf.info.path,
			Records:  records,
			FileHash: cf.info.hash,
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
