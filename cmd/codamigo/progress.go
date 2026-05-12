package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"golang.org/x/term"

	"github.com/ieshan/codamigo/indexer"
)

// renderTickMsg carries an atomic snapshot of progress counters for one render tick.
type renderTickMsg struct {
	processed int64
	skipped   int64
	current   string
}

// indexDoneMsg is sent once after indexing completes, carrying final counts.
type indexDoneMsg struct {
	processed int64
	skipped   int64
}

// progressReporter implements indexer.Progress using lock-free atomic counters.
// All methods are safe for concurrent use from any number of goroutines.
type progressReporter struct {
	processed atomic.Int64
	skipped   atomic.Int64
	// current stores a string; Load() returns nil before the first FileProcessed
	// call, not "" — see currentFile().
	current atomic.Value
}

// compile-time interface check
var _ indexer.Progress = (*progressReporter)(nil)

func (r *progressReporter) FileProcessed(path string) {
	r.processed.Add(1)
	r.current.Store(path)
}

func (r *progressReporter) FileSkipped(_ string) {
	r.skipped.Add(1)
}

// currentFile reads the atomic string safely. atomic.Value.Load() returns nil
// (not "") before the first Store — a direct type assertion on nil would panic.
func (r *progressReporter) currentFile() string {
	if v := r.current.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// runTicker sends a renderTickMsg to the program every 100 ms until done is closed.
// Hard-caps Bubble Tea message volume at 10 messages/second regardless of indexing rate.
func runTicker(p *tea.Program, r *progressReporter, done <-chan struct{}) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.Send(renderTickMsg{
				processed: r.processed.Load(),
				skipped:   r.skipped.Load(),
				current:   r.currentFile(),
			})
		case <-done:
			return
		}
	}
}

// processingPrefix is the fixed label on line 2 of the progress display.
// Declared as a const so the available path width calculation in View() stays in sync.
const processingPrefix = "Processing: "

// doneStyle is a package-level var to avoid per-render heap allocations.
var doneStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.ANSIColor(2))

// progressModel is the Bubble Tea model for the indexing progress display.
type progressModel struct {
	processed   int64
	skipped     int64
	currentFile string
	width       int
	done        bool
	cancel      context.CancelFunc // called on ctrl+c to stop the indexer; nil-safe
}

func (m progressModel) Init() tea.Cmd { return nil }

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case renderTickMsg:
		if m.done {
			return m, nil
		}
		m.processed = msg.processed
		m.skipped = msg.skipped
		m.currentFile = msg.current
	case indexDoneMsg:
		m.processed = msg.processed
		m.skipped = msg.skipped
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m progressModel) View() tea.View {
	if m.done {
		return tea.NewView(doneStyle.Render(fmt.Sprintf("Done. Processed: %d   Skipped: %d", m.processed, m.skipped)) + "\n")
	}
	line1 := fmt.Sprintf("Processed: %d   Skipped: %d", m.processed, m.skipped)
	if m.currentFile == "" {
		return tea.NewView(line1)
	}
	line2 := processingPrefix + truncatePath(m.currentFile, m.width-len(processingPrefix))
	return tea.NewView(line1 + "\n" + line2)
}

// truncatePath returns path truncated to maxWidth runes, prefixed with "…" when
// truncation occurs. Returns "" when maxWidth <= 0.
func truncatePath(path string, maxWidth int) string {
	if maxWidth <= 0 {
		return ""
	}
	runes := []rune(path)
	if len(runes) <= maxWidth {
		return path
	}
	return "…" + string(runes[len(runes)-maxWidth+1:])
}

// newProgressTUI returns a ready-to-run Bubble Tea program and its reporter when
// stderr is a TTY. Returns nil, nil in non-TTY environments (CI, pipes).
//
// cancel is called when the user presses ctrl+c. In TTY mode the terminal is in
// raw mode and ISIG is cleared, so ctrl+c arrives as a KeyPressMsg rather than
// SIGINT; the model must forward it to the indexer's context explicitly.
func newProgressTUI(cancel context.CancelFunc) (*tea.Program, *progressReporter) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return nil, nil
	}
	p := tea.NewProgram(
		progressModel{cancel: cancel},
		tea.WithOutput(os.Stderr),
		tea.WithoutSignalHandler(),
	)
	return p, &progressReporter{}
}
