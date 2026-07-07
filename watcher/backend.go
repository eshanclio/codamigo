package watcher

import "errors"

// fsOp is a bitmask of filesystem operations, mirroring the internal
// representation used by OS-level notification backends (kqueue, inotify).
// It is distinct from the public [Op] type, which is a simple enum consumed
// by the wrapper layer.
type fsOp uint32

const (
	fsCreate fsOp = 1 << iota
	fsWrite
	fsRemove
	fsRename
	fsChmod
)

// Has reports whether this fsOp bitmask contains the given operation.
func (o fsOp) Has(h fsOp) bool { return o&h != 0 }

// fsEvent is the internal event type produced by OS-level backends.
// It is converted to the public [Event] by the fsnotify wrapper.
type fsEvent struct {
	Name string
	Op   fsOp
}

// Has reports whether this event has the given operation.
func (e fsEvent) Has(op fsOp) bool { return e.Op.Has(op) }

// backend is the internal interface implemented by each platform-specific
// notification backend (kqueue, inotify). Event and error channels are
// returned separately from newBackendWatcher, so the interface only covers
// watch management and lifecycle.
type backend interface {
	Add(path string) error
	Remove(path string) error
	Close() error
}

// Sentinel errors for watcher backend conditions.
var (
	ErrEventOverflow    = errors.New("watcher: queue or buffer overflow")
	ErrNonExistentWatch = errors.New("watcher: can't remove non-existent watch")
	ErrClosed           = errors.New("watcher: watcher already closed")
)

// newBackendWatcher creates a new OS-level backend, returning the backend
// along with its event and error channels. The specific backend is selected
// by build tags.
func newBackendWatcher() (backend, <-chan fsEvent, <-chan error, error) {
	ev := make(chan fsEvent)
	errs := make(chan error)
	b, err := newBackend(ev, errs)
	if err != nil {
		return nil, nil, nil, err
	}
	return b, ev, errs, nil
}
