package watcher

import "sync"

// shared provides common channel coordination and close signalling for
// all backend implementations. It is embedded by kqueueWatcher and
// inotifyWatcher.
type shared struct {
	events chan fsEvent
	errors chan error
	done   chan struct{}
	mu     sync.Mutex
}

func newShared(ev chan fsEvent, errs chan error) *shared {
	return &shared{
		events: ev,
		errors: errs,
		done:   make(chan struct{}),
	}
}

// sendEvent sends an event on the events channel. It returns false if the
// watcher has been closed. Events with a zero Op are silently dropped.
func (s *shared) sendEvent(e fsEvent) bool {
	if e.Op == 0 {
		return true
	}
	select {
	case <-s.done:
		return false
	case s.events <- e:
		return true
	}
}

// sendError sends an error on the errors channel. It returns false if the
// watcher has been closed. nil errors are silently dropped.
func (s *shared) sendError(err error) bool {
	if err == nil {
		return true
	}
	select {
	case <-s.done:
		return false
	case s.errors <- err:
		return true
	}
}

// isClosed reports whether the watcher has been closed.
func (s *shared) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// close marks the watcher as closed. It returns true if it was already
// closed. Safe to call from any goroutine.
func (s *shared) close() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed() {
		return true
	}
	close(s.done)
	return false
}
