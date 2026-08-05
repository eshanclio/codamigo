//go:build linux && !appengine

package watcher

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/unix"
)

// inotifyWatcher implements the [backend] interface using Linux's inotify
// kernel subsystem. Unlike kqueue, inotify watches directories (not
// individual files) and reports events for children automatically.
type inotifyWatcher struct {
	*shared

	fd          int
	inotifyFile *os.File
	watches     *iwatches
	doneResp    chan struct{}
}

type (
	iwatches struct {
		wd   map[uint32]*iwatch // wd → watch
		path map[string]uint32  // pathname → wd
	}
	iwatch struct {
		wd   uint32
		path string
	}
)

func newIwatches() *iwatches {
	return &iwatches{
		wd:   make(map[uint32]*iwatch),
		path: make(map[string]uint32),
	}
}

func (w *iwatches) byWd(wd uint32) *iwatch { return w.wd[wd] }
func (w *iwatches) add(ww *iwatch)         { w.wd[ww.wd] = ww; w.path[ww.path] = ww.wd }
func (w *iwatches) remove(watch *iwatch)   { delete(w.path, watch.path); delete(w.wd, watch.wd) }

// newBackend creates a new inotify-based backend.
func newBackend(ev chan fsEvent, errs chan error) (backend, error) {
	fd, errno := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if fd == -1 {
		return nil, fmt.Errorf("couldn't initialize inotify: %w", errno)
	}

	w := &inotifyWatcher{
		shared:      newShared(ev, errs),
		fd:          fd,
		inotifyFile: os.NewFile(uintptr(fd), ""),
		watches:     newIwatches(),
		doneResp:    make(chan struct{}),
	}

	go w.readEvents()
	return w, nil
}

func (w *inotifyWatcher) Close() error {
	if w.close() {
		return nil
	}

	err := w.inotifyFile.Close()
	if err != nil {
		return err
	}

	<-w.doneResp
	return nil
}

func (w *inotifyWatcher) Add(name string) error {
	if w.isClosed() {
		return ErrClosed
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	const flags = unix.IN_CREATE |
		unix.IN_MODIFY |
		unix.IN_DELETE |
		unix.IN_DELETE_SELF |
		unix.IN_MOVED_TO |
		unix.IN_MOVED_FROM |
		unix.IN_MOVE_SELF |
		unix.IN_ATTRIB

	return w.register(name, flags)
}

func (w *inotifyWatcher) register(path string, flags uint32) error {
	wd, err := unix.InotifyAddWatch(w.fd, path, flags)
	if wd == -1 {
		return err
	}

	if _, ok := w.watches.wd[uint32(wd)]; ok { // #nosec G115 -- wd is an inotify watch descriptor, checked != -1 above; always small and non-negative
		return nil
	}

	w.watches.add(&iwatch{
		wd:   uint32(wd), // #nosec G115 -- wd is an inotify watch descriptor, checked != -1 above; always small and non-negative
		path: path,
	})
	return nil
}

func (w *inotifyWatcher) Remove(name string) error {
	if w.isClosed() {
		return nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	wd, ok := w.watches.path[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNonExistentWatch, name)
	}

	_, err := unix.InotifyRmWatch(w.fd, wd)
	if err != nil {
		return err
	}

	delete(w.watches.path, name)
	delete(w.watches.wd, wd)
	return nil
}

// readEvents reads from the inotify file descriptor, converts the received
// events into fsEvent objects and sends them via the events channel.
func (w *inotifyWatcher) readEvents() {
	defer func() {
		close(w.doneResp)
		close(w.events)
		close(w.errors)
	}()

	var buf [unix.SizeofInotifyEvent * 4096]byte
	for {
		if w.isClosed() {
			return
		}

		n, err := w.inotifyFile.Read(buf[:])
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			if !w.sendError(err) {
				return
			}
			continue
		}

		if n < unix.SizeofInotifyEvent {
			if n == 0 {
				err = io.EOF
			} else {
				err = errors.New("watcher: short read in readEvents()")
			}
			if !w.sendError(err) {
				return
			}
			continue
		}

		// n is a byte count from Read() into a fixed 4096-event buffer,
		// checked >= SizeofInotifyEvent above; never near uint32 range.
		limit := uint32(n - unix.SizeofInotifyEvent) // #nosec G115
		var offset uint32
		for offset <= limit {
			inEvent := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset])) // #nosec G103 -- standard way to decode inotify's binary event stream; offset is bounds-checked by the loop condition above

			if inEvent.Mask&unix.IN_Q_OVERFLOW != 0 {
				if !w.sendError(ErrEventOverflow) {
					return
				}
			}

			ev, ok, herr := w.handleEvent(inEvent, &buf, offset)
			if !ok {
				return
			}
			if herr != nil {
				if !w.sendError(herr) {
					return
				}
			}
			if !w.sendEvent(ev) {
				return
			}

			offset += unix.SizeofInotifyEvent + inEvent.Len
		}
	}
}

func (w *inotifyWatcher) handleEvent(inEvent *unix.InotifyEvent, buf *[65536]byte, offset uint32) (fsEvent, bool, error) {
	w.mu.Lock()

	watch := w.watches.byWd(uint32(inEvent.Wd)) // #nosec G115 -- inEvent.Wd mirrors a watch descriptor this process itself registered; always small and non-negative
	if watch == nil {
		w.mu.Unlock()
		return fsEvent{}, true, nil
	}

	var (
		name    = watch.path
		nameLen = uint32(inEvent.Len)
	)
	if nameLen > 0 {
		name += "/" + inotifyEventName(buf, offset, nameLen)
	}

	if inEvent.Mask&unix.IN_IGNORED != 0 || inEvent.Mask&unix.IN_UNMOUNT != 0 {
		w.watches.remove(watch)
		w.mu.Unlock()
		return fsEvent{}, true, nil
	}

	// inotify will automatically remove the watch on deletes; just need
	// to clean our state here.
	if inEvent.Mask&unix.IN_DELETE_SELF == unix.IN_DELETE_SELF {
		w.watches.remove(watch)
	}

	// We can't really update the state when a watched path is moved; only
	// IN_MOVE_SELF is sent and not IN_MOVED_{FROM,TO}. So remove the watch.
	// Inline the removal rather than calling w.Remove to avoid re-acquiring
	// w.mu (which we already hold) — sync.Mutex is not reentrant.
	var moveErr error
	if inEvent.Mask&unix.IN_MOVE_SELF == unix.IN_MOVE_SELF {
		_, err := unix.InotifyRmWatch(w.fd, watch.wd)
		if err != nil && !errors.Is(err, unix.EINVAL) {
			moveErr = err
		}
		w.watches.remove(watch)
	}

	// Skip if we're watching both this path and the parent; the parent will
	// already send a delete so no need to do it twice.
	if inEvent.Mask&unix.IN_DELETE_SELF != 0 {
		_, ok := w.watches.path[filepath.Dir(watch.path)]
		if ok {
			w.mu.Unlock()
			return fsEvent{}, true, nil
		}
	}

	ev := w.newEvent(name, inEvent.Mask)
	w.mu.Unlock()

	// Send error outside the lock to avoid deadlock with Close() which
	// needs the same mutex to signal shutdown via shared.close().
	if moveErr != nil {
		return ev, true, moveErr
	}
	return ev, true, nil
}

func inotifyEventName(buf *[65536]byte, offset, nameLen uint32) string {
	start := int(offset + unix.SizeofInotifyEvent)
	// #nosec G103 -- casting the raw read buffer to a fixed-size byte array is the standard way to read the variable-length name inotify appends after each event; start/nameLen come from the kernel-reported event length, bounds-checked by readEvents' loop
	bytes := (*[unix.PathMax]byte)(unsafe.Pointer(&buf[start]))[:nameLen:nameLen]
	for nameLen > 0 && bytes[nameLen-1] == 0 {
		nameLen--
	}
	return string(bytes[:nameLen])
}

// newEvent returns an fsEvent based on the inotify event mask.
func (w *inotifyWatcher) newEvent(name string, mask uint32) fsEvent {
	e := fsEvent{Name: name}
	if mask&unix.IN_CREATE == unix.IN_CREATE || mask&unix.IN_MOVED_TO == unix.IN_MOVED_TO {
		e.Op |= fsCreate
	}
	if mask&unix.IN_DELETE_SELF == unix.IN_DELETE_SELF || mask&unix.IN_DELETE == unix.IN_DELETE {
		e.Op |= fsRemove
	}
	if mask&unix.IN_MODIFY == unix.IN_MODIFY {
		e.Op |= fsWrite
	}
	if mask&unix.IN_MOVE_SELF == unix.IN_MOVE_SELF || mask&unix.IN_MOVED_FROM == unix.IN_MOVED_FROM {
		e.Op |= fsRename
	}
	if mask&unix.IN_ATTRIB == unix.IN_ATTRIB {
		e.Op |= fsChmod
	}
	return e
}
