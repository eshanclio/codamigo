package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/ieshan/codamigo/config"
)

// pollWatcher implements [Watcher] by periodically scanning the filesystem
// and comparing mod-times between scans. It is the fallback when kernel-level
// watching is unavailable or the OS watch limit has been reached.
type pollWatcher struct {
	root            string
	fsys            fs.FS
	baseInterval    time.Duration
	idleStreak      int // consecutive idle polls; used to back off the scan rate
	includePatterns []string
	excludePatterns []string
	matchFn         func(string) bool
	current         map[string]time.Time // abs path → mod time from latest scan
	prev            map[string]time.Time // abs path → mod time from previous scan
	done            chan struct{}
}

// newPollWatcher creates a pollWatcher using the interval and filter settings
// from cfg. The watcher does not start polling until Watch is called.
func newPollWatcher(cfg *config.Config, matchFn func(string) bool, fsys fs.FS) *pollWatcher {
	interval := cfg.PollInterval
	if interval == 0 {
		interval = 5 * time.Second
	}
	return &pollWatcher{
		root:            cfg.ProjectRoot,
		fsys:            fsys,
		baseInterval:    interval,
		includePatterns: cfg.IncludePatterns,
		excludePatterns: cfg.ExcludePatterns,
		matchFn:         matchFn,
		current:         make(map[string]time.Time),
		prev:            make(map[string]time.Time),
		done:            make(chan struct{}),
	}
}

// nextInterval returns the poll interval for the next scan. After consecutive
// idle polls (no events) the interval grows exponentially up to 4× baseInterval,
// reducing CPU load in quiet repositories.
func (p *pollWatcher) nextInterval() time.Duration {
	if p.idleStreak == 0 {
		return p.baseInterval
	}
	shift := min(p.idleStreak, 3)
	multiplier := min(1<<shift, 4)
	return p.baseInterval * time.Duration(multiplier)
}

func (p *pollWatcher) Watch(ctx context.Context) <-chan []Event {
	ch := make(chan []Event)

	p.scan()
	// p.prev is intentionally left empty after the initial scan.
	// On the first poll, p.current (from scan) is swapped into p.prev and
	// re-scanned into p.current. Pre-existing files then appear in both maps,
	// so they do NOT generate Create events — only genuinely new changes do.
	p.prev = make(map[string]time.Time, len(p.current))

	go func() {
		defer close(ch)
		timer := time.NewTimer(p.baseInterval)
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-p.done:
				return
			case <-timer.C:
				events := p.poll()
				if len(events) > 0 {
					p.idleStreak = 0
					select {
					case ch <- events:
					case <-ctx.Done():
						return
					case <-p.done:
						return
					}
				} else {
					p.idleStreak++
				}
				timer.Reset(p.nextInterval())
			}
		}
	}()

	return ch
}

func (p *pollWatcher) Close() error {
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	return nil
}

func (p *pollWatcher) scan() {
	if err := fs.WalkDir(p.fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("poll: scan entry error", slog.String("path", rel), slog.Any("error", err))
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(rel)
			if base == ".git" || base == ".codamigo" {
				return fs.SkipDir
			}
			return nil
		}
		absPath := filepath.Join(p.root, rel)
		if !p.shouldInclude(absPath) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			slog.Warn("poll: scan info error", slog.String("path", rel), slog.Any("error", err))
			return nil
		}
		p.current[absPath] = info.ModTime()
		return nil
	}); err != nil {
		slog.Warn("poll: scan walk error", slog.String("root", p.root), slog.Any("error", err))
	}
}

func (p *pollWatcher) poll() []Event {
	// Two-map swap: reuse allocations, clear the map that becomes current.
	p.prev, p.current = p.current, p.prev
	clear(p.current)

	if err := fs.WalkDir(p.fsys, ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("poll: entry error", slog.String("path", rel), slog.Any("error", err))
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(rel)
			if base == ".git" || base == ".codamigo" {
				return fs.SkipDir
			}
			return nil
		}
		absPath := filepath.Join(p.root, rel)
		if !p.shouldInclude(absPath) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			slog.Warn("poll: info error", slog.String("path", rel), slog.Any("error", err))
			return nil
		}
		p.current[absPath] = info.ModTime()
		return nil
	}); err != nil {
		slog.Warn("poll: walk error", slog.String("root", p.root), slog.Any("error", err))
	}

	var events []Event

	// Single-pass: detect creates/modifies and remove seen keys from prev.
	for path, modTime := range p.current {
		prevTime, existed := p.prev[path]
		if !existed {
			events = append(events, Event{Path: path, Op: Create})
			slog.Debug("poll: new file", "path", path)
		} else if modTime.After(prevTime) {
			events = append(events, Event{Path: path, Op: Write})
			slog.Debug("poll: modified file", "path", path)
		}
		delete(p.prev, path)
	}

	// Whatever remains in prev was not seen in current — deleted.
	for path := range p.prev {
		events = append(events, Event{Path: path, Op: Remove})
		slog.Debug("poll: removed file", "path", path)
	}

	return events
}

// shouldInclude reports whether path should produce events.
// When matchFn is set it takes precedence; otherwise the include/exclude
// pattern lists are consulted.
func (p *pollWatcher) shouldInclude(path string) bool {
	if p.matchFn != nil {
		return p.matchFn(path)
	}
	return p.matchPatterns(path)
}

func (p *pollWatcher) matchPatterns(path string) bool {
	rel, err := filepath.Rel(p.root, path)
	if err != nil {
		return false
	}
	base := filepath.Base(rel)

	for _, pattern := range p.excludePatterns {
		if matched, _ := filepath.Match(pattern, base); matched {
			return false
		}
		if matched, _ := filepath.Match(pattern, rel); matched {
			return false
		}
	}

	if len(p.includePatterns) > 0 {
		for _, pattern := range p.includePatterns {
			if matched, _ := filepath.Match(pattern, base); matched {
				return true
			}
			if matched, _ := filepath.Match(pattern, rel); matched {
				return true
			}
		}
		return false
	}

	return true
}
