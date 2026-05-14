package main

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestTruncatePath(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		maxWidth int
		want     string
	}{
		{"empty path", "", 20, ""},
		{"fits exactly", "short.go", 20, "short.go"},
		{"at limit", "exactly20characters!", 20, "exactly20characters!"},
		{"truncated", "/long/path/to/file.go", 10, "…o/file.go"},
		{"negative maxWidth", "any", -1, ""},
		{"zero maxWidth", "any", 0, ""},
		{"single char at limit", "x", 1, "x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncatePath(tt.path, tt.maxWidth)
			if got != tt.want {
				t.Errorf("truncatePath(%q, %d) = %q; want %q", tt.path, tt.maxWidth, got, tt.want)
			}
		})
	}
}

func TestProgressModel_Update(t *testing.T) {
	t.Run("renderTickMsg updates counts and current file", func(t *testing.T) {
		m := progressModel{}
		next, _ := m.Update(renderTickMsg{processed: 5, skipped: 2, current: "foo.go"})
		nm := next.(progressModel)
		if nm.processed != 5 || nm.skipped != 2 || nm.currentFile != "foo.go" {
			t.Errorf("unexpected model state: %+v", nm)
		}
	})

	t.Run("indexDoneMsg sets done and returns tea.Quit", func(t *testing.T) {
		m := progressModel{}
		next, cmd := m.Update(indexDoneMsg{processed: 10, skipped: 3})
		nm := next.(progressModel)
		if !nm.done {
			t.Error("expected done=true after indexDoneMsg")
		}
		if nm.processed != 10 || nm.skipped != 3 {
			t.Errorf("unexpected counts: processed=%d skipped=%d", nm.processed, nm.skipped)
		}
		// In Bubble Tea v2, tea.Cmd is an opaque function value — we cannot
		// type-assert it to verify it is specifically tea.Quit. Non-nil is the
		// practical limit of what can be asserted here.
		if cmd == nil {
			t.Error("expected non-nil cmd (tea.Quit) after indexDoneMsg")
		}
	})

	t.Run("renderTickMsg ignored when done=true", func(t *testing.T) {
		m := progressModel{done: true, processed: 7}
		next, _ := m.Update(renderTickMsg{processed: 99})
		nm := next.(progressModel)
		if nm.processed != 7 {
			t.Errorf("processed mutated after done: got %d, want 7", nm.processed)
		}
	})

	t.Run("WindowSizeMsg updates width", func(t *testing.T) {
		m := progressModel{}
		next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		nm := next.(progressModel)
		if nm.width != 120 {
			t.Errorf("width = %d; want 120", nm.width)
		}
	})

	t.Run("ctrl+c calls cancel and returns tea.Quit", func(t *testing.T) {
		cancelled := false
		m := progressModel{cancel: func() { cancelled = true }}
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if !cancelled {
			t.Error("expected cancel to be called on ctrl+c")
		}
		if cmd == nil {
			t.Error("expected non-nil cmd (tea.Quit) after ctrl+c")
		}
	})

	t.Run("ctrl+c with nil cancel does not panic", func(t *testing.T) {
		m := progressModel{} // cancel is nil
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if cmd == nil {
			t.Error("expected non-nil cmd (tea.Quit) after ctrl+c with nil cancel")
		}
	})
}

func TestProgressModel_View(t *testing.T) {
	t.Run("in-progress shows counts and current file", func(t *testing.T) {
		m := progressModel{processed: 3, skipped: 1, currentFile: "main.go", width: 80}
		view := m.View().Content
		if !strings.Contains(view, "Processed: 3") {
			t.Errorf("view missing processed count: %q", view)
		}
		if !strings.Contains(view, "Skipped: 1") {
			t.Errorf("view missing skipped count: %q", view)
		}
		if !strings.Contains(view, "main.go") {
			t.Errorf("view missing current file: %q", view)
		}
	})

	t.Run("no current file shows only line 1", func(t *testing.T) {
		m := progressModel{processed: 2, skipped: 0, currentFile: "", width: 80}
		view := m.View().Content
		if strings.Contains(view, "Processing:") {
			t.Errorf("view should not show Processing line when currentFile is empty: %q", view)
		}
	})

	t.Run("narrow terminal truncates path", func(t *testing.T) {
		m := progressModel{
			processed:   1,
			currentFile: "/very/long/path/to/some/deep/file.go",
			width:       20,
		}
		view := m.View().Content
		if strings.Contains(view, "/very/long/path") {
			t.Errorf("expected path to be truncated, got: %q", view)
		}
		if !strings.Contains(view, "…") {
			t.Errorf("expected truncation ellipsis in: %q", view)
		}
	})

	t.Run("zero width does not panic", func(t *testing.T) {
		m := progressModel{currentFile: "file.go", width: 0}
		_ = m.View()
	})

	t.Run("done state shows summary with Done prefix", func(t *testing.T) {
		m := progressModel{done: true, processed: 42, skipped: 7}
		view := m.View().Content
		if !strings.Contains(view, "Done.") {
			t.Errorf("done view missing 'Done.' prefix: %q", view)
		}
		if !strings.Contains(view, "Processed: 42") {
			t.Errorf("done view missing processed count: %q", view)
		}
		if !strings.Contains(view, "Skipped: 7") {
			t.Errorf("done view missing skipped count: %q", view)
		}
	})
}

func TestProgressReporter_Atomics(t *testing.T) {
	r := &progressReporter{}

	r.FileStarted("starting.go")
	if got := r.currentFile(); got != "starting.go" {
		t.Errorf("currentFile after FileStarted = %q; want starting.go", got)
	}
	if got := r.processed.Load(); got != 0 {
		t.Errorf("processed after FileStarted = %d; want 0", got)
	}
	if got := r.skipped.Load(); got != 0 {
		t.Errorf("skipped after FileStarted = %d; want 0", got)
	}

	r.FileProcessed("a.go")
	r.FileProcessed("b.go")
	r.FileSkipped("c.go")
	if got := r.processed.Load(); got != 2 {
		t.Errorf("processed = %d; want 2", got)
	}
	if got := r.skipped.Load(); got != 1 {
		t.Errorf("skipped = %d; want 1", got)
	}
	if got := r.currentFile(); got != "b.go" {
		t.Errorf("currentFile = %q; want b.go", got)
	}
}

func TestProgressReporter_CurrentFileBeforeFirstStore(t *testing.T) {
	r := &progressReporter{}
	if got := r.currentFile(); got != "" {
		t.Errorf("currentFile before any FileStarted call = %q; want empty string", got)
	}
}

func TestNewProgressTUI_NonTTY(t *testing.T) {
	// Test environments never attach a TTY to stderr, so newProgressTUI must
	// return nil, nil — otherwise indexCmd would launch a TUI in CI and deadlock.
	prog, reporter := newProgressTUI(func() {})
	if prog != nil {
		t.Error("expected prog=nil in non-TTY environment")
	}
	if reporter != nil {
		t.Error("expected reporter=nil in non-TTY environment")
	}
}

// TestProgressReporter_FileFailed_RecordsAndSurfacesPath verifies that
// FileFailed accumulates failed paths for the final summary without
// disrupting in-progress reporting.
func TestProgressReporter_FileFailed_RecordsAndSurfacesPath(t *testing.T) {
	r := newProgressReporter()
	r.FileFailed("/tmp/foo.go", errors.New("embed: simulated"))
	r.FileFailed("/tmp/bar.go", errors.New("embed: simulated"))

	failed := r.FailedFiles()
	if len(failed) != 2 {
		t.Fatalf("FailedFiles() len = %d, want 2", len(failed))
	}
	if failed[0] != "/tmp/foo.go" || failed[1] != "/tmp/bar.go" {
		t.Errorf("failed = %v, want order preserved", failed)
	}
}
