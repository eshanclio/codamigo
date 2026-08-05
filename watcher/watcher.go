// Package watcher monitors a directory tree for filesystem changes and
// delivers batched, debounced events to callers.
//
// [Watcher] is the interface; [New] selects an implementation based on
// [config.Config.WatchMode]: "fsnotify" for kernel-level notifications
// (kqueue on macOS/BSD, inotify on Linux), "poll" for periodic directory
// scanning, or "auto" which tries kernel-level watching and falls back to
// polling. [Event] carries the changed path and [Op] type.
package watcher

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/ieshan/codamigo/config"
)

// Op is the type of filesystem change.
type Op int

const (
	// Create signals a new file was created.
	Create Op = iota
	// Write signals an existing file was modified.
	Write
	// Remove signals a file was deleted.
	Remove
	// Rename signals a file was renamed or moved.
	Rename
	// Reindex signals that the watcher detected a potential event loss
	// (e.g. inotify queue overflow) and a full re-index should be performed
	// to recover potentially missed changes.
	Reindex
)

func (o Op) String() string {
	switch o {
	case Create:
		return "create"
	case Write:
		return "write"
	case Remove:
		return "remove"
	case Rename:
		return "rename"
	case Reindex:
		return "reindex"
	default:
		return fmt.Sprintf("Op(%d)", int(o))
	}
}

// Event describes a single filesystem change.
type Event struct {
	Path string
	Op   Op
}

// Watcher monitors a directory tree for file changes.
// Watch must be called exactly once; calling it a second time is not safe.
type Watcher interface {
	// Watch returns a channel of batched, debounced events.
	// The channel is closed when ctx is cancelled or Close is called.
	// Watch must be called exactly once per Watcher instance.
	Watch(ctx context.Context) <-chan []Event
	Close() error
}

// New creates a Watcher based on cfg.WatchMode:
//   - "poll": polling watcher
//   - "fsnotify": kernel-level watcher (errors if unavailable)
//   - "auto" or "": attempts kernel-level watching; falls back to polling on
//     error, watch limit, or failed event-delivery probe
//
// matchFn, when non-nil, is called to decide whether a file path should
// produce events. Pass nil to use the include/exclude pattern logic only.
// fsys is the filesystem to walk; pass w.FS() (the walker's os.Root-scoped FS)
// for production use to prevent symlink-escape attacks.
func New(cfg *config.Config, matchFn func(string) bool, fsys fs.FS) (Watcher, error) {
	switch cfg.WatchMode {
	case "poll":
		return newPollWatcher(cfg, matchFn, fsys), nil
	case "fsnotify":
		fw, _, err := newFSNotifyWatcher(cfg, matchFn, fsys)
		return fw, err
	case "auto", "":
		fw, watchLimitHit, err := newFSNotifyWatcher(cfg, matchFn, fsys)
		if err != nil {
			slog.Warn("fsnotify unavailable, falling back to polling", slog.Any("error", err))
			return newPollWatcher(cfg, matchFn, fsys), nil
		}
		if watchLimitHit {
			_ = fw.Close() // best-effort cleanup; we fall back to the poll watcher either way
			slog.Warn("watch limit reached, falling back to polling",
				slog.String("hint", "on Linux: increase fs.inotify.max_user_watches; on macOS: increase ulimit -n"))
			return newPollWatcher(cfg, matchFn, fsys), nil
		}
		if !fw.probe(2 * time.Second) {
			_ = fw.Close() // best-effort cleanup; we fall back to the poll watcher either way
			slog.Warn("fsnotify probe failed — events not delivered; falling back to polling",
				slog.String("hint", "this may indicate a Docker bind mount or network filesystem; set watch_mode: \"poll\" to skip this probe"))
			return newPollWatcher(cfg, matchFn, fsys), nil
		}
		return fw, nil
	default:
		return nil, fmt.Errorf("unknown watch mode %q", cfg.WatchMode)
	}
}
