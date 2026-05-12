package watcher

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ieshan/codamigo/config"
)

// fsnotifyWatcher implements [Watcher] using kernel-level filesystem
// notifications via the fsnotify library. It registers all directories under
// the project root with the OS and delivers debounced, batched events.
type fsnotifyWatcher struct {
	fw       *fsnotify.Watcher
	root     string
	fsys     fs.FS
	debounce time.Duration
	matchFn  func(string) bool
}

// newFSNotifyWatcher creates an fsnotifyWatcher and registers all directories
// under cfg.ProjectRoot with the OS watcher. The second return value is true
// when a watch-limit error was encountered while registering directories; the
// caller should fall back to polling in that case. The returned watcher must
// be closed via Close when no longer needed.
func newFSNotifyWatcher(cfg *config.Config, matchFn func(string) bool, fsys fs.FS) (*fsnotifyWatcher, bool, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, false, err
	}

	debounce := cfg.DebounceWindow
	if debounce == 0 {
		debounce = 500 * time.Millisecond
	}

	w := &fsnotifyWatcher{
		fw:       fw,
		root:     cfg.ProjectRoot,
		fsys:     fsys,
		debounce: debounce,
		matchFn:  matchFn,
	}

	watchLimitHit, err := w.addDirs()
	if err != nil {
		fw.Close()
		return nil, false, err
	}

	return w, watchLimitHit, nil
}

// addDirs walks root and registers every directory with the fsnotify watcher.
// It returns watchLimitHit=true when a watch-limit syscall error is encountered,
// in which case the walk is aborted early. matchFn is intentionally NOT applied
// here — directories must always be watched to detect new file creation.
func (w *fsnotifyWatcher) addDirs() (watchLimitHit bool, err error) {
	err = fs.WalkDir(w.fsys, ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			slog.Warn("fsnotify: walk entry error", slog.String("path", rel), slog.Any("error", walkErr))
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		name := filepath.Base(rel)
		if name == ".git" || name == ".codamigo" {
			return fs.SkipDir
		}
		absPath := filepath.Join(w.root, rel)
		if addErr := w.fw.Add(absPath); addErr != nil {
			if IsWatchLimitError(addErr) {
				watchLimitHit = true
				return fs.SkipAll
			}
			slog.Warn("fsnotify: failed to watch directory",
				slog.String("path", absPath), slog.Any("error", addErr))
		}
		return nil
	})
	return watchLimitHit, err
}

// IsWatchLimitError reports whether err is a OS-level watch-limit error.
// On Linux this is ENOSPC (inotify watch table full); on macOS it is EMFILE
// (per-process file descriptor limit) or ENFILE (system-wide limit).
func IsWatchLimitError(err error) bool {
	return errors.Is(err, syscall.ENOSPC) ||
		errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE)
}

func (w *fsnotifyWatcher) Watch(ctx context.Context) <-chan []Event {
	ch := make(chan []Event)

	go func() {
		defer close(ch)
		pending := make(map[string]Event)
		timer := time.NewTimer(w.debounce)
		timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-w.fw.Events:
				if !ok {
					return
				}
				event := w.convertEvent(ev)
				if event == nil {
					continue
				}
				if ev.Op&fsnotify.Create != 0 {
					w.maybeAddDir(ev.Name)
				}
				pending[event.Path] = *event
				timer.Reset(w.debounce)

			case err, ok := <-w.fw.Errors:
				if !ok {
					return
				}
				slog.Warn("fsnotify error", slog.Any("error", err))

			case <-timer.C:
				if len(pending) == 0 {
					continue
				}
				batch := make([]Event, 0, len(pending))
				for _, e := range pending {
					batch = append(batch, e)
				}
				clear(pending)
				select {
				case ch <- batch:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return ch
}

func (w *fsnotifyWatcher) Close() error {
	return w.fw.Close()
}

func (w *fsnotifyWatcher) convertEvent(ev fsnotify.Event) *Event {
	// Skip events from .codamigo directory to prevent feedback loops.
	for _, part := range strings.Split(filepath.ToSlash(ev.Name), "/") {
		if part == ".codamigo" {
			return nil
		}
	}
	var op Op
	switch {
	case ev.Op&fsnotify.Remove != 0:
		op = Remove
	case ev.Op&fsnotify.Rename != 0:
		op = Rename
	case ev.Op&fsnotify.Create != 0:
		op = Create
	case ev.Op&fsnotify.Write != 0:
		op = Write
	default:
		return nil
	}

	// For non-remove/rename events, skip directories.
	if op != Remove && op != Rename {
		relPath, relErr := filepath.Rel(w.root, ev.Name)
		if relErr == nil {
			info, statErr := fs.Stat(w.fsys, relPath)
			if statErr == nil && info.IsDir() {
				return nil
			}
		}
	}

	// Filter through matchFn if provided. Always report Remove/Rename so the
	// indexer can clean up stale records even for files it previously excluded.
	if w.matchFn != nil && op != Remove && op != Rename {
		if !w.matchFn(ev.Name) {
			return nil
		}
	}

	return &Event{Path: ev.Name, Op: op}
}

func (w *fsnotifyWatcher) maybeAddDir(path string) {
	relPath, relErr := filepath.Rel(w.root, path)
	if relErr != nil {
		return
	}
	info, err := fs.Stat(w.fsys, relPath)
	if err != nil || !info.IsDir() {
		return
	}
	if name := filepath.Base(path); name == ".git" || name == ".codamigo" {
		return
	}
	if err = w.fw.Add(path); err != nil {
		slog.Warn("fsnotify: failed to watch new directory", slog.String("path", path), slog.Any("error", err))
	}
}
