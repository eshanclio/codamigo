package watcher_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/watcher"
)

func TestPollWatcher_DetectsNewFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "existing.go"), []byte("package main"), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg := &config.Config{
			ProjectRoot:  root,
			WatchMode:    "poll",
			PollInterval: 200 * time.Millisecond,
		}

		w, err := watcher.New(cfg, nil, os.DirFS(root))
		if err != nil {
			t.Fatalf("new watcher: %v", err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()

		ch := w.Watch(ctx)

		// Wait for first poll to complete (captures initial state).
		synctest.Wait()

		// Create a new file.
		if err = os.WriteFile(filepath.Join(root, "new.go"), []byte("package main\nfunc new() {}"), 0o644); err != nil {
			t.Fatal(err)
		}

		select {
		case events := <-ch:
			found := false
			for _, e := range events {
				if filepath.Base(e.Path) == "new.go" && e.Op == watcher.Create {
					found = true
				}
			}
			if !found {
				t.Errorf("expected Create event for new.go, got %v", events)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for events")
		}
	})
}

func TestPollWatcher_DetectsModification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "main.go")
		if err := os.WriteFile(path, []byte("package main"), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg := &config.Config{
			ProjectRoot:  root,
			WatchMode:    "poll",
			PollInterval: 200 * time.Millisecond,
		}

		w, err := watcher.New(cfg, nil, os.DirFS(root))
		if err != nil {
			t.Fatalf("new watcher: %v", err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()

		ch := w.Watch(ctx)
		synctest.Wait()

		// Modify the file (ensure different ModTime).
		synctest.Wait()
		if err = os.WriteFile(path, []byte("package main\nfunc modified() {}"), 0o644); err != nil {
			t.Fatal(err)
		}

		select {
		case events := <-ch:
			found := false
			for _, e := range events {
				if filepath.Base(e.Path) == "main.go" && e.Op == watcher.Write {
					found = true
				}
			}
			if !found {
				t.Errorf("expected Write event for main.go, got %v", events)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for events")
		}
	})
}

func TestPollWatcher_DetectsRemoval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "delete_me.go")
		if err := os.WriteFile(path, []byte("package main"), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg := &config.Config{
			ProjectRoot:  root,
			WatchMode:    "poll",
			PollInterval: 200 * time.Millisecond,
		}

		w, err := watcher.New(cfg, nil, os.DirFS(root))
		if err != nil {
			t.Fatalf("new watcher: %v", err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()

		ch := w.Watch(ctx)
		synctest.Wait()

		if err = os.Remove(path); err != nil {
			t.Fatal(err)
		}

		select {
		case events := <-ch:
			found := false
			for _, e := range events {
				if filepath.Base(e.Path) == "delete_me.go" && e.Op == watcher.Remove {
					found = true
				}
			}
			if !found {
				t.Errorf("expected Remove event for delete_me.go, got %v", events)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for events")
		}
	})
}

func TestPollWatcher_ContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		cfg := &config.Config{
			ProjectRoot:  root,
			WatchMode:    "poll",
			PollInterval: 100 * time.Millisecond,
		}

		w, err := watcher.New(cfg, nil, os.DirFS(root))
		if err != nil {
			t.Fatalf("new watcher: %v", err)
		}
		defer w.Close()

		ctx, cancel := context.WithCancel(t.Context())
		ch := w.Watch(ctx)
		cancel()

		// Channel should be closed after cancellation.
		synctest.Wait()
		select {
		case _, ok := <-ch:
			if ok {
				// May receive one last batch before close, drain and check.
				synctest.Wait()
				_, ok = <-ch
				if ok {
					t.Error("expected channel to be closed after context cancellation")
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for channel close")
		}
	})
}

func TestFSNotifyWatcher_DetectsNewFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		ProjectRoot:    root,
		WatchMode:      "fsnotify",
		DebounceWindow: 200 * time.Millisecond,
	}

	w, err := watcher.New(cfg, nil, os.DirFS(root))
	if err != nil {
		t.Skipf("fsnotify not available: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	ch := w.Watch(ctx)

	// Give the watcher time to set up watches.
	time.Sleep(200 * time.Millisecond)

	if err = os.WriteFile(filepath.Join(root, "new.go"), []byte("package main\nfunc new() {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case events := <-ch:
		found := false
		for _, e := range events {
			if filepath.Base(e.Path) == "new.go" {
				found = true
			}
		}
		if !found {
			t.Errorf("expected event for new.go, got %v", events)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for events")
	}
}

func TestFSNotifyWatcher_DetectsRemoval(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "delete_me.go")
	if err := os.WriteFile(path, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		ProjectRoot:    root,
		WatchMode:      "fsnotify",
		DebounceWindow: 200 * time.Millisecond,
	}

	w, err := watcher.New(cfg, nil, os.DirFS(root))
	if err != nil {
		t.Skipf("fsnotify not available: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	ch := w.Watch(ctx)
	time.Sleep(200 * time.Millisecond)

	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}

	select {
	case events := <-ch:
		found := false
		for _, e := range events {
			if filepath.Base(e.Path) == "delete_me.go" && (e.Op == watcher.Remove || e.Op == watcher.Rename) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected Remove event for delete_me.go, got %v", events)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for events")
	}
}

func TestPollWatcher_AdaptiveInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "test.go"), []byte("package x"), 0o644) //nolint:errcheck

		cfg := &config.Config{
			ProjectRoot:  root,
			WatchMode:    "poll",
			PollInterval: 100 * time.Millisecond,
		}

		w, err := watcher.New(cfg, nil, os.DirFS(root))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		ch := w.Watch(ctx)

		// Wait for a few poll cycles with no changes.
		// The watcher should not send any events.
		synctest.Wait()

		// Now create a new file — should trigger an event.
		os.WriteFile(filepath.Join(root, "new.go"), []byte("package y"), 0o644) //nolint:errcheck

		select {
		case events := <-ch:
			if len(events) == 0 {
				t.Error("expected non-empty events")
			}
			found := false
			for _, e := range events {
				if strings.HasSuffix(e.Path, "new.go") {
					found = true
				}
			}
			if !found {
				t.Error("expected event for new.go")
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for event")
		}
	})
}

func TestFSNotifyWatcher_DetectsModification(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		ProjectRoot:    root,
		WatchMode:      "fsnotify",
		DebounceWindow: 200 * time.Millisecond,
	}

	w, err := watcher.New(cfg, nil, os.DirFS(root))
	if err != nil {
		t.Skipf("fsnotify not available: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	ch := w.Watch(ctx)
	time.Sleep(200 * time.Millisecond)

	if err = os.WriteFile(path, []byte("package main\nfunc modified() {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case events := <-ch:
		found := false
		for _, e := range events {
			if filepath.Base(e.Path) == "main.go" && (e.Op == watcher.Write || e.Op == watcher.Create) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected Write event for main.go, got %v", events)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for events")
	}
}

func TestPollWatcher_MatchFnFilters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root := t.TempDir()

		// matchFn rejects .log files, accepts everything else.
		matchFn := func(path string) bool {
			return !strings.HasSuffix(path, ".log")
		}

		cfg := &config.Config{
			ProjectRoot:  root,
			WatchMode:    "poll",
			PollInterval: 200 * time.Millisecond,
		}

		w, err := watcher.New(cfg, matchFn, os.DirFS(root))
		if err != nil {
			t.Fatalf("new watcher: %v", err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		ch := w.Watch(ctx)

		// Wait for the initial scan to complete before writing files, so both
		// files are seen as new on the first poll (not pre-existing).
		synctest.Wait()

		// Write both files atomically before any poll fires, so neither is
		// missed due to a race between two separate poll cycles.
		if err = os.WriteFile(filepath.Join(root, "ignored.log"), []byte("log data"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Advance virtual time past a poll cycle so the watcher picks up changes.
		time.Sleep(600 * time.Millisecond)
		synctest.Wait()

		// Drain all events that arrived during the wait window.
		var allEvents []watcher.Event
	collectLoop:
		for {
			select {
			case batch := <-ch:
				allEvents = append(allEvents, batch...)
			default:
				break collectLoop
			}
		}

		// No .log events should have been delivered.
		for _, e := range allEvents {
			if strings.HasSuffix(e.Path, ".log") {
				t.Errorf("matchFn should have filtered out .log file, but got event: %v", e)
			}
		}

		// A Create event for main.go must be present.
		found := false
		for _, e := range allEvents {
			if strings.HasSuffix(e.Path, "main.go") && e.Op == watcher.Create {
				found = true
			}
		}
		if !found {
			t.Errorf("expected Create event for main.go, got %v", allEvents)
		}
	})
}

func TestIsWatchLimitError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ENOSPC", syscall.ENOSPC, true},
		{"EMFILE", syscall.EMFILE, true},
		{"ENFILE", syscall.ENFILE, true},
		{"wrapped ENOSPC", fmt.Errorf("watch failed: %w", syscall.ENOSPC), true},
		{"other error", fmt.Errorf("something else"), false},
		{"nil", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := watcher.IsWatchLimitError(tc.err)
			if got != tc.want {
				t.Errorf("IsWatchLimitError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
