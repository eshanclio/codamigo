// Package walker walks a directory tree yielding file paths that pass
// ignore rules (.gitignore and .caignore) and include/exclude glob filters from [config.Config].
//
// [Walker.Walk] returns an [iter.Seq2] iterator so callers process files
// one at a time without holding the full list in memory. Context cancellation
// stops the walk early. walker imports only config; it knows nothing about
// chunking, embedding, or storage.
package walker

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ieshan/codamigo/config"
)

// ignoreLayer pairs a directory with the parsed .gitignore rules from that directory.
// The dir field is relative to the walker root.
type ignoreLayer struct {
	dir   string // relative to walker root (e.g. "." or "src" or "src/vendor")
	rules *gitIgnore
}

// ignoreStack manages a stack of ignore layers for nested .gitignore/.caignore evaluation.
// Layers are pushed as directories are entered during a walk and popped when
// leaving a subtree. The cache avoids re-parsing the same ignore files on repeated
// calls to IsIgnored or Match.
//
// cacheMu protects cache; it is shared (via pointer) between the Walker's main
// ignoreStack and any local stacks created by Match/IsIgnored so concurrent
// cache reads and writes are safe.
type ignoreStack struct {
	layers  []ignoreLayer
	cache   map[string]*gitIgnore // keyed by relative dir; guarded by cacheMu
	cacheMu *sync.RWMutex
}

func newIgnoreStack() *ignoreStack {
	return &ignoreStack{
		cache:   make(map[string]*gitIgnore),
		cacheMu: &sync.RWMutex{},
	}
}

func (s *ignoreStack) push(relDir string, gi *gitIgnore) {
	s.layers = append(s.layers, ignoreLayer{dir: filepath.ToSlash(relDir), rules: gi})
}

func (s *ignoreStack) popTo(currentRelDir string) {
	cur := filepath.ToSlash(currentRelDir)
	for len(s.layers) > 0 {
		top := s.layers[len(s.layers)-1]
		topDir := filepath.ToSlash(top.dir)
		// Root layer ("." ) always applies everywhere.
		if topDir == "." {
			break
		}
		if cur == topDir || strings.HasPrefix(cur, topDir+"/") {
			break
		}
		s.layers = s.layers[:len(s.layers)-1]
	}
}

func (s *ignoreStack) alreadyInStack(relDir string) bool {
	for _, layer := range s.layers {
		if layer.dir == relDir {
			return true
		}
	}
	return false
}

func (s *ignoreStack) loadDir(ctx context.Context, fsys fs.FS, relDir string) {
	if s.alreadyInStack(relDir) {
		return
	}

	// Fast path: check cache under read lock.
	s.cacheMu.RLock()
	gi, cached := s.cache[relDir]
	s.cacheMu.RUnlock()

	if cached {
		if gi != nil {
			s.push(relDir, gi)
		}
		return
	}

	// Parse outside any lock — pure computation on the FS.
	lines := loadIgnoreLines(ctx, fsys, relDir)

	// Write the result under write lock (another goroutine may race here; last
	// writer wins, both produce identical results so this is safe).
	s.cacheMu.Lock()
	if _, alreadyCached := s.cache[relDir]; !alreadyCached {
		if len(lines) == 0 {
			s.cache[relDir] = nil
		} else {
			gi = compileGitIgnore(lines)
			s.cache[relDir] = gi
		}
	} else {
		gi = s.cache[relDir]
	}
	s.cacheMu.Unlock()

	if gi != nil {
		s.push(relDir, gi)
	}
}

func (s *ignoreStack) match(relPath string, isDir bool) bool {
	result := matchNone
	for _, layer := range s.layers {
		layerRel, err := filepath.Rel(layer.dir, relPath)
		if err != nil || strings.HasPrefix(layerRel, "..") {
			continue
		}
		layerRel = filepath.ToSlash(layerRel)
		if m := layer.rules.MatchResult(layerRel, isDir); m != matchNone {
			result = m
		}
	}
	return result == matchIgnored
}

// Option configures a Walker at construction time.
type Option func(*Walker)

// WithFileFilter attaches a path predicate to the Walker. fn receives the
// file path relative to the walker root and must return true for the file to
// be yielded. A nil fn (or omitting this option) preserves the existing
// behaviour: all paths that pass gitignore and include/exclude rules are yielded.
func WithFileFilter(fn func(string) bool) Option {
	return func(w *Walker) {
		if fn != nil {
			w.fileFilter = fn
		}
	}
}

// Walker walks a directory tree, yielding file paths that pass all configured filters.
type Walker struct {
	root            string
	fsRoot          atomic.Pointer[os.Root]
	includePatterns []string
	excludePatterns []string
	ignore          *ignoreStack
	ignoreMu        sync.Mutex        // held exclusively by Walk for the duration of a full directory walk; never acquired by Match or IsIgnored
	fileFilter      func(string) bool // nil means accept all; written once at New, read-only after
}

// New creates a Walker rooted at root using filter configuration from cfg.
// The caller should call Close when done to release the underlying file descriptor.
func New(root string, cfg *config.Config, opts ...Option) (*Walker, error) {
	fsRoot, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("opening root: %w", err)
	}
	w := &Walker{
		root:            root,
		includePatterns: cfg.IncludePatterns,
		excludePatterns: cfg.ExcludePatterns,
		ignore:          newIgnoreStack(),
	}
	w.fsRoot.Store(fsRoot)
	for _, opt := range opts {
		opt(w)
	}
	return w, nil
}

// FS returns an fs.FS scoped to the walker's root directory.
// The returned FS prevents path traversal via symlinks or ../ escapes.
func (w *Walker) FS() fs.FS {
	if r := w.fsRoot.Load(); r != nil {
		return r.FS()
	}
	panic("walker: FS called after Close")
}

// Close releases the os.Root file descriptor.
func (w *Walker) Close() error {
	if old := w.fsRoot.Swap(nil); old != nil {
		return old.Close()
	}
	return nil
}

// Root returns the absolute path of the walker's root directory.
func (w *Walker) Root() string {
	return w.root
}

// Walk yields absolute file paths that satisfy the include/exclude patterns and
// gitignore rules. Stops early if ctx is cancelled.
func (w *Walker) Walk(ctx context.Context) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		w.ignoreMu.Lock()
		defer w.ignoreMu.Unlock()

		// Load root ignore files before walking.
		fsys := w.FS()
		w.ignore.loadDir(ctx, fsys, ".")

		if err := fs.WalkDir(fsys, ".", func(rel string, d fs.DirEntry, err error) error {
			if err != nil {
				if !yield("", err) {
					return fs.SkipAll
				}
				return nil
			}

			if ctx.Err() != nil {
				return fs.SkipAll
			}

			if d.IsDir() {
				if w.shouldSkipDir(rel, w.ignore) {
					return fs.SkipDir
				}
				// Pop layers that no longer apply and load this dir's .gitignore.
				w.ignore.popTo(rel)
				if rel != "." {
					w.ignore.loadDir(ctx, fsys, rel)
				}
				return nil
			}

			if d.Type()&fs.ModeSymlink != 0 {
				info, statErr := fs.Stat(fsys, rel)
				if statErr != nil {
					slog.DebugContext(ctx, "skipping dangling symlink",
						slog.String("path", filepath.Join(w.root, rel)),
						slog.Any("error", statErr))
					return nil
				}
				if info.IsDir() {
					slog.DebugContext(ctx, "skipping symlink to directory",
						slog.String("path", filepath.Join(w.root, rel)))
					return nil
				}
				// Symlink to a regular file — fall through to matchFile.
			}

			if !w.matchFile(rel, w.ignore) {
				return nil
			}

			absPath := filepath.Join(w.root, rel)
			if !yield(absPath, nil) {
				return fs.SkipAll
			}
			return nil
		}); err != nil {
			yield("", err)
		}
	}
}

// Match reports whether the file at the given absolute path would be yielded by Walk.
// Safe for concurrent use — builds a local ignore stack without locking ignoreMu.
func (w *Walker) Match(absPath string) bool {
	rel, err := filepath.Rel(w.root, absPath)
	if err != nil {
		return false
	}

	local := &ignoreStack{cache: w.ignore.cache, cacheMu: w.ignore.cacheMu}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	fsys := w.FS()
	local.loadDir(context.Background(), fsys, ".")
	for i := range len(parts) - 1 {
		dirRel := filepath.Join(parts[:i+1]...)
		local.loadDir(context.Background(), fsys, dirRel)
	}

	for i := range len(parts) - 1 {
		dirRel := filepath.Join(parts[:i+1]...)
		if w.shouldSkipDir(dirRel, local) {
			return false
		}
	}
	return w.matchFile(rel, local)
}

func (w *Walker) shouldSkipDir(rel string, ig *ignoreStack) bool {
	if rel == "." {
		return false
	}
	// Always skip .git and .codamigo at any level.
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".git" || part == ".codamigo" {
			return true
		}
	}
	return ig.match(rel, true)
}

func (w *Walker) matchFile(rel string, ig *ignoreStack) bool {
	base := filepath.Base(rel)

	// Skip ignore files — they are walker metadata, not source.
	if base == ".gitignore" || base == ".caignore" {
		return false
	}

	if ig.match(rel, false) {
		return false
	}

	for _, pattern := range w.excludePatterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return false
		}
		if matched, _ := filepath.Match(pattern, rel); matched {
			return false
		}
	}

	if len(w.includePatterns) > 0 {
		included := false
		for _, pattern := range w.includePatterns {
			if matched, _ := filepath.Match(pattern, base); matched {
				included = true
				break
			}
			if matched, _ := filepath.Match(pattern, rel); matched {
				included = true
				break
			}
		}
		if !included {
			return false
		}
	}

	if w.fileFilter != nil && !w.fileFilter(rel) {
		return false
	}
	return true
}

// IsIgnored reports whether the absolute path should be ignored based on gitignore rules.
// Safe for concurrent use — builds a local ignore stack without locking ignoreMu.
func (w *Walker) IsIgnored(absPath string) bool {
	rel, err := filepath.Rel(w.root, absPath)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)

	local := &ignoreStack{cache: w.ignore.cache, cacheMu: w.ignore.cacheMu}
	parts := strings.Split(rel, "/")
	fsys := w.FS()
	local.loadDir(context.Background(), fsys, ".")
	for i := range len(parts) - 1 {
		relDir := filepath.Join(parts[:i+1]...)
		local.loadDir(context.Background(), fsys, relDir)
	}

	// Check if any ancestor directory is ignored (handles dirOnly patterns like "vendor/").
	for i := range len(parts) - 1 {
		dirRel := filepath.ToSlash(filepath.Join(parts[:i+1]...))
		if local.match(dirRel, true) {
			return true
		}
	}

	return local.match(rel, false)
}

func readIgnoreFile(ctx context.Context, fsys fs.FS, filePath string) []string {
	f, err := fsys.Open(filePath)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }() // best-effort cleanup; the file is only being read

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err = scanner.Err(); err != nil {
		slog.WarnContext(ctx, "reading ignore file", slog.String("path", filePath), slog.Any("error", err))
		return lines
	}
	return lines
}

func loadIgnoreLines(ctx context.Context, fsys fs.FS, relDir string) []string {
	var lines []string
	for _, name := range []string{".gitignore", ".caignore"} {
		var p string
		if relDir == "." {
			p = name
		} else {
			p = path.Join(relDir, name)
		}
		lines = append(lines, readIgnoreFile(ctx, fsys, p)...)
	}
	return lines
}
