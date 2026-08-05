package watcher

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ieshan/codamigo/config"
)

// fsnotifyWatcher implements [Watcher] using kernel-level filesystem
// notifications (kqueue on macOS/BSD, inotify on Linux). It registers all
// directories under the project root with the OS and delivers debounced,
// batched events.
type fsnotifyWatcher struct {
	bw       backend
	events   <-chan fsEvent
	errors   <-chan error
	root     string
	fsys     fs.FS
	debounce time.Duration
	matchFn  func(string) bool
	probeFn  func(timeout time.Duration) bool
}

// newFSNotifyWatcher creates an fsnotifyWatcher and registers all directories
// under cfg.ProjectRoot with the OS watcher. The second return value is true
// when a watch-limit error was encountered while registering directories; the
// caller should fall back to polling in that case. The returned watcher must
// be closed via Close when no longer needed.
func newFSNotifyWatcher(cfg *config.Config, matchFn func(string) bool, fsys fs.FS) (*fsnotifyWatcher, bool, error) {
	bw, events, errors, err := newBackendWatcher()
	if err != nil {
		return nil, false, err
	}

	debounce := cfg.DebounceWindow
	if debounce == 0 {
		debounce = 500 * time.Millisecond
	}

	w := &fsnotifyWatcher{
		bw:       bw,
		events:   events,
		errors:   errors,
		root:     cfg.ProjectRoot,
		fsys:     fsys,
		debounce: debounce,
		matchFn:  matchFn,
	}

	watchLimitHit, err := w.addDirs()
	if err != nil {
		_ = bw.Close() // best-effort cleanup; the addDirs error below is the one worth reporting
		return nil, false, err
	}

	return w, watchLimitHit, nil
}

// addDirs walks root and registers every directory with the OS watcher.
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
		if addErr := w.bw.Add(absPath); addErr != nil {
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

// IsWatchLimitError reports whether err is an OS-level watch-limit error.
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
			case ev, ok := <-w.events:
				if !ok {
					return
				}
				if ev.Op.Has(fsCreate) {
					w.maybeAddDir(ev.Name)
				}
				if ev.Op.Has(fsRemove) || ev.Op.Has(fsRename) {
					w.maybeRemoveDir(ev.Name)
				}
				event := w.convertEvent(ev)
				if event == nil {
					continue
				}
				pending[event.Path] = *event
				timer.Reset(w.debounce)

			case err, ok := <-w.errors:
				if !ok {
					return
				}
				if errors.Is(err, ErrEventOverflow) {
					slog.Warn("fsnotify: event queue overflow detected; triggering full re-index",
						slog.String("hint", "on Linux: increase fs.inotify.max_queued_events"))
					pending["__reindex__"] = Event{Op: Reindex}
					timer.Reset(w.debounce)
				} else {
					slog.Warn("fsnotify error", slog.Any("error", err))
				}

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
	return w.bw.Close()
}

func (w *fsnotifyWatcher) convertEvent(ev fsEvent) *Event {
	// Skip events from .codamigo directory to prevent feedback loops.
	for _, part := range strings.Split(filepath.ToSlash(ev.Name), "/") {
		if part == ".codamigo" || part == ".watchprobe" {
			return nil
		}
	}
	var op Op
	switch {
	case ev.Op.Has(fsRemove):
		op = Remove
	case ev.Op.Has(fsRename):
		op = Rename
	case ev.Op.Has(fsCreate):
		op = Create
	case ev.Op.Has(fsWrite):
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

func (w *fsnotifyWatcher) maybeRemoveDir(path string) {
	if name := filepath.Base(path); name == ".git" || name == ".codamigo" {
		return
	}
	if err := w.bw.Remove(path); err != nil {
		if !errors.Is(err, ErrNonExistentWatch) {
			slog.Warn("fsnotify: failed to remove watch for deleted directory",
				slog.String("path", path), slog.Any("error", err))
		}
	}
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
	if err = w.bw.Add(path); err != nil {
		slog.Warn("fsnotify: failed to watch new directory", slog.String("path", path), slog.Any("error", err))
	}
}

// probe verifies that the kernel watcher is actually delivering events by
// creating a temporary file and waiting for the corresponding Create event.
// Returns true if the event was received within the timeout. This detects
// environments where kernel notifications silently fail (e.g. Docker bind
// mounts on macOS).
//
// probe must be called before Watch; it has exclusive access to the
// events channel because the event loop goroutine has not started yet.
func (w *fsnotifyWatcher) probe(timeout time.Duration) bool {
	if w.probeFn != nil {
		return w.probeFn(timeout)
	}
	probePath := filepath.Join(w.root, ".watchprobe")
	defer func() { _ = os.Remove(probePath) }() // best-effort cleanup of the throwaway probe file

	if err := os.WriteFile(probePath, []byte("probe"), 0o600); err != nil {
		slog.Warn("fsnotify probe: cannot create probe file", slog.Any("error", err))
		return true
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-w.events:
			if !ok {
				return false
			}
			if ev.Op.Has(fsCreate) && strings.HasSuffix(ev.Name, ".watchprobe") {
				return true
			}
		case <-deadline.C:
			return false
		}
	}
}
