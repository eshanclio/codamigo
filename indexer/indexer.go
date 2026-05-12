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
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/ieshan/codamigo/chunker"
	"github.com/ieshan/codamigo/embedder"
	"github.com/ieshan/codamigo/store"
	"github.com/ieshan/codamigo/walker"
)

// Progress receives per-file outcome notifications during indexing.
// Implementations must be safe for concurrent use — indexFile runs inside
// errgroup goroutines.
type Progress interface {
	// FileProcessed is called after a file has been successfully embedded and stored.
	FileProcessed(path string)
	// FileSkipped is called when a file is bypassed due to a hash match (content
	// unchanged), an unsupported language extension, or exceeding maxFileSize.
	FileSkipped(path string)
}

// Indexer orchestrates the walk → chunk → embed → store pipeline.
type Indexer struct {
	chunker     *chunker.Chunker
	embedder    embedder.Embedder
	store       store.Store
	walker      *walker.Walker
	fsys        fs.FS
	concurrency int
	maxFileSize int64
	onIndexed   func()
	progress    Progress
}

// New constructs an Indexer from its dependencies.
// c may be nil for hash-only indexing (no chunking); when non-nil it must have
// been built with the desired language configs (from langs.AllLanguages() in cmd/).
// indexer does not import langs directly.
// The caller retains ownership of w and must close it separately; the indexer
// does not close the walker.
// concurrency controls the maximum number of files processed in parallel;
// values <= 0 are treated as 1.
// progress is optional; pass nil to disable per-file notifications.
func New(c *chunker.Chunker, e embedder.Embedder, s store.Store, w *walker.Walker, concurrency int, maxFileSize int64, onIndexed func(), progress Progress) *Indexer {
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
	return &Indexer{
		chunker:     c,
		embedder:    e,
		store:       s,
		walker:      w,
		fsys:        w.FS(),
		concurrency: concurrency,
		maxFileSize: maxFileSize,
		onIndexed:   onIndexed,
		progress:    progress,
	}
}

// Index walks the project root, chunks every matched file, embeds each chunk,
// and upserts the resulting records into the store. Files whose content hash
// matches the stored hash are skipped. Records for files that no longer exist
// on disk are removed from the store. Returns nil on context cancellation
// (partial progress is left in the store and is valid).
func (idx *Indexer) Index(ctx context.Context) error {
	walked := make(map[string]struct{})

	var g errgroup.Group
	g.SetLimit(idx.concurrency)

	for path, err := range idx.walker.Walk(ctx) {
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			return fmt.Errorf("walking: %w", err)
		}
		walked[path] = struct{}{}
		g.Go(func() error {
			if err := idx.indexFile(ctx, path); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.ErrorContext(ctx, "indexing file failed", slog.String("path", path), slog.Any("error", err))
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	indexed, err := idx.store.ListFiles(ctx)
	if err != nil {
		return fmt.Errorf("listing indexed files: %w", err)
	}
	for _, path := range indexed {
		if _, ok := walked[path]; !ok {
			if err := idx.store.DeleteByFile(ctx, path); err != nil {
				return fmt.Errorf("deleting removed file %s: %w", path, err)
			}
		}
	}

	if err := idx.store.Checkpoint(ctx); err != nil {
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
	var g errgroup.Group
	g.SetLimit(idx.concurrency)

	for _, path := range paths {
		g.Go(func() error {
			if ctx.Err() != nil {
				return ctx.Err()
			}

			relPath, relErr := filepath.Rel(idx.walker.Root(), path)
			outsideRoot := relErr != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator))

			if outsideRoot {
				// File is outside the project root — can't index it, but
				// still clean up any stale records from a prior index.
				if err := idx.store.DeleteByFile(ctx, path); err != nil {
					slog.ErrorContext(ctx, "deleting out-of-root file failed", slog.String("path", path), slog.Any("error", err))
				}
				return nil
			}

			_, err := fs.Stat(idx.fsys, relPath)
			if errors.Is(err, fs.ErrNotExist) {
				if err := idx.store.DeleteByFile(ctx, path); err != nil {
					slog.ErrorContext(ctx, "deleting missing file failed", slog.String("path", path), slog.Any("error", err))
				}
				return nil
			}
			if err != nil {
				slog.ErrorContext(ctx, "stat failed", slog.String("path", path), slog.Any("error", err))
				return nil
			}

			if !idx.walker.Match(path) {
				return nil
			}

			if err := idx.indexFile(ctx, path); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				slog.ErrorContext(ctx, "indexing file failed", slog.String("path", path), slog.Any("error", err))
			}
			return nil
		})
	}

	err := g.Wait()
	if err == nil && idx.onIndexed != nil {
		idx.onIndexed()
	}
	return err
}

func (idx *Indexer) indexFile(ctx context.Context, path string) error {
	relPath, err := filepath.Rel(idx.walker.Root(), path)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		slog.WarnContext(ctx, "skipping file outside project root", slog.String("path", path))
		return nil
	}

	content, err := fs.ReadFile(idx.fsys, relPath)
	if err != nil {
		return fmt.Errorf("reading file: %w", err)
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

	fileHash := store.ContentHash(content)
	existingHash, err := idx.store.FileHash(ctx, path)
	if err != nil {
		return fmt.Errorf("checking file hash: %w", err)
	}
	if fileHash == existingHash {
		if idx.progress != nil {
			idx.progress.FileSkipped(path)
		}
		return nil
	}

	var chunks []chunker.Chunk
	if idx.chunker != nil {
		chunks, err = idx.chunker.ChunkFile(path, content)
		if errors.Is(err, chunker.ErrUnsupportedLanguage) {
			slog.DebugContext(ctx, "skipping unsupported file type", slog.String("path", path))
			if idx.progress != nil {
				idx.progress.FileSkipped(path)
			}
			return idx.store.SetFileHash(ctx, path, fileHash)
		}
		if err != nil {
			return fmt.Errorf("chunking: %w", err)
		}
	}

	if len(chunks) == 0 {
		if err := idx.store.DeleteByFile(ctx, path); err != nil {
			return fmt.Errorf("deleting stale chunks for %s: %w", path, err)
		}
		if err := idx.store.SetFileHash(ctx, path, fileHash); err != nil {
			return err
		}
		if idx.progress != nil {
			idx.progress.FileProcessed(path)
		}
		return nil
	}

	contentHashes := make([]string, len(chunks))
	for i, ch := range chunks {
		contentHashes[i] = store.ContentHash([]byte(ch.Content))
	}

	cachedEmbeddings, err := idx.store.EmbeddingsByContentHash(ctx, contentHashes)
	if err != nil {
		return fmt.Errorf("fetching cached embeddings: %w", err)
	}

	uncachedTexts := make([]string, 0, len(chunks))
	for i, ch := range chunks {
		if _, ok := cachedEmbeddings[contentHashes[i]]; !ok {
			uncachedTexts = append(uncachedTexts, ch.Content)
		}
	}

	var newEmbeddings [][]float32
	if len(uncachedTexts) > 0 {
		newEmbeddings, err = idx.embedder.EmbedBatch(ctx, uncachedTexts)
		if err != nil {
			return fmt.Errorf("embedding: %w", err)
		}
		if len(newEmbeddings) != len(uncachedTexts) {
			return fmt.Errorf("embedder returned %d embeddings for %d texts", len(newEmbeddings), len(uncachedTexts))
		}
	}

	records := make([]store.Record, len(chunks))
	newIdx := 0
	for i, ch := range chunks {
		var emb []float32
		if cached, ok := cachedEmbeddings[contentHashes[i]]; ok {
			emb = cached
		} else {
			emb = newEmbeddings[newIdx]
			newIdx++
		}

		records[i] = store.Record{
			ID:          store.RecordID(path, ch.Content),
			FilePath:    path,
			Language:    ch.Language,
			Content:     ch.Content,
			ContentHash: contentHashes[i],
			NodeKind:    ch.NodeKind,
			Name:        ch.Name,
			Parent:      ch.Parent,
			StartLine:   ch.Start.Line,
			EndLine:     ch.End.Line,
			Embedding:   emb,
		}
	}

	if err := idx.store.ReplaceByFile(ctx, path, records, fileHash); err != nil {
		return err
	}
	if idx.progress != nil {
		idx.progress.FileProcessed(path)
	}
	return nil
}
