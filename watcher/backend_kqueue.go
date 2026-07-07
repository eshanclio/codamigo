//go:build darwin || freebsd || openbsd || netbsd || dragonfly

package watcher

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"golang.org/x/sys/unix"
)

// kqueueWatcher implements the [backend] interface using kqueue/kevent
// (macOS, BSD). Unlike inotify, kqueue requires an open file descriptor
// per watched path, not just per directory.
type kqueueWatcher struct {
	*shared

	kq        int    // File descriptor (as returned by kqueue()).
	closepipe [2]int // Pipe used to wake the event loop on Close.
	watches   *kwatches
}

type (
	kwatches struct {
		mu     sync.RWMutex
		wd     map[int]kwatch              // wd → watch
		path   map[string]int              // pathname → wd
		byDir  map[string]map[int]struct{} // dirname(path) → wd set
		seen   map[string]struct{}         // Known-to-exist tracking.
		byUser map[string]struct{}         // Watches added via Add().
	}
	kwatch struct {
		wd       int
		name     string
		linkName string // For symlinks: name is target, linkName is the link.
		isDir    bool
		dirFlags uint32
	}
)

func newKwatches() *kwatches {
	return &kwatches{
		wd:     make(map[int]kwatch),
		path:   make(map[string]int),
		byDir:  make(map[string]map[int]struct{}),
		seen:   make(map[string]struct{}),
		byUser: make(map[string]struct{}),
	}
}

func (w *kwatches) listPaths() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	l := make([]string, 0, len(w.path))
	for p := range w.path {
		l = append(l, p)
	}
	return l
}

func (w *kwatches) watchesInDir(path string) []string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	l := make([]string, 0, 4)
	for fd := range w.byDir[path] {
		info := w.wd[fd]
		if _, ok := w.byUser[info.name]; !ok {
			l = append(l, info.name)
		}
	}
	return l
}

func (w *kwatches) addUserWatch(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.byUser[path] = struct{}{}
}

func (w *kwatches) addLink(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.path[path] = 0
	w.seen[path] = struct{}{}
}

func (w *kwatches) add(path, linkPath string, fd int, isDir bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.path[path] = fd
	w.wd[fd] = kwatch{wd: fd, name: path, linkName: linkPath, isDir: isDir}

	parent := filepath.Dir(path)
	byDir, ok := w.byDir[parent]
	if !ok {
		byDir = make(map[int]struct{}, 1)
		w.byDir[parent] = byDir
	}
	byDir[fd] = struct{}{}
}

func (w *kwatches) byWd(fd int) (kwatch, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	info, ok := w.wd[fd]
	return info, ok
}

func (w *kwatches) byPath(path string) (kwatch, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	info, ok := w.wd[w.path[path]]
	return info, ok
}

func (w *kwatches) updateDirFlags(path string, flags uint32) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	fd, ok := w.path[path]
	if !ok {
		return false
	}
	info := w.wd[fd]
	info.dirFlags = flags
	w.wd[fd] = info
	return true
}

func (w *kwatches) remove(fd int, path string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	isDir := w.wd[fd].isDir
	delete(w.path, path)
	delete(w.byUser, path)

	parent := filepath.Dir(path)
	delete(w.byDir[parent], fd)

	if len(w.byDir[parent]) == 0 {
		delete(w.byDir, parent)
	}

	delete(w.wd, fd)
	delete(w.seen, path)
	return isDir
}

func (w *kwatches) markSeen(path string, exists bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if exists {
		w.seen[path] = struct{}{}
	} else {
		delete(w.seen, path)
	}
}

func (w *kwatches) seenBefore(path string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	_, ok := w.seen[path]
	return ok
}

// newBackend creates a new kqueue-based backend.
func newBackend(ev chan fsEvent, errs chan error) (backend, error) {
	kq, closepipe, err := newKqueue()
	if err != nil {
		return nil, err
	}

	w := &kqueueWatcher{
		shared:    newShared(ev, errs),
		kq:        kq,
		closepipe: closepipe,
		watches:   newKwatches(),
	}

	go w.readEvents()
	return w, nil
}

// newKqueue creates a new kernel event queue and returns a descriptor.
// A closepipe is registered so that kevent() can be woken up on Close.
func newKqueue() (kq int, closepipe [2]int, err error) {
	kq, err = unix.Kqueue()
	if err != nil {
		return kq, closepipe, err
	}

	err = unix.Pipe(closepipe[:])
	if err != nil {
		unix.Close(kq)
		return kq, closepipe, err
	}
	unix.CloseOnExec(closepipe[0])
	unix.CloseOnExec(closepipe[1])

	changes := make([]unix.Kevent_t, 1)
	unix.SetKevent(&changes[0], closepipe[0], unix.EVFILT_READ,
		unix.EV_ADD|unix.EV_ENABLE|unix.EV_ONESHOT)

	ok, err := unix.Kevent(kq, changes, nil, nil)
	if ok == -1 {
		unix.Close(kq)
		unix.Close(closepipe[0])
		unix.Close(closepipe[1])
		return kq, closepipe, err
	}
	return kq, closepipe, nil
}

func (w *kqueueWatcher) Close() error {
	if w.shared.close() {
		return nil
	}

	// Snapshot and drop all watches directly. w.remove short-circuits on
	// isClosed() (which is already true after w.shared.close() above), so
	// calling Remove here would leak every watched fd. On macOS a single
	// directory watch opens an fd for every file in the dir, so
	// long-running processes that recreate watchers would run out of fds
	// with EMFILE.
	pathsToRemove := w.watches.listPaths()
	for _, name := range pathsToRemove {
		info, ok := w.watches.byPath(name)
		if !ok {
			w.watches.remove(0, name)
			continue
		}
		_ = w.register([]int{info.wd}, unix.EV_DELETE, 0)
		unix.Close(info.wd)
		w.watches.remove(info.wd, name)
	}

	unix.Close(w.closepipe[1]) // Send "quit" message to readEvents
	return nil
}

func (w *kqueueWatcher) Add(name string) error {
	_, err := w.addWatch(name, noteAllEvents, false)
	if err != nil {
		return err
	}
	w.watches.addUserWatch(name)
	return nil
}

func (w *kqueueWatcher) Remove(name string) error {
	return w.remove(name, true)
}

func (w *kqueueWatcher) remove(name string, unwatchFiles bool) error {
	if w.isClosed() {
		return nil
	}

	name = filepath.Clean(name)
	info, ok := w.watches.byPath(name)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNonExistentWatch, name)
	}

	err := w.register([]int{info.wd}, unix.EV_DELETE, 0)
	if err != nil {
		return err
	}

	unix.Close(info.wd)

	isDir := w.watches.remove(info.wd, name)

	if unwatchFiles && isDir {
		pathsToRemove := w.watches.watchesInDir(name)
		for _, p := range pathsToRemove {
			w.Remove(p)
		}
	}
	return nil
}

// noteAllEvents is the bitmask of all kqueue filter flags we watch for
// (except NOTE_EXTEND, NOTE_LINK, NOTE_REVOKE).
const noteAllEvents = unix.NOTE_DELETE | unix.NOTE_WRITE | unix.NOTE_ATTRIB | unix.NOTE_RENAME

// addWatch adds name to the watched file set; the flags are interpreted as
// described in kevent(2). Returns the real path to the file, with symlinks
// resolved.
func (w *kqueueWatcher) addWatch(name string, flags uint32, listDir bool) (string, error) {
	if w.isClosed() {
		return "", ErrClosed
	}

	name = filepath.Clean(name)

	info, alreadyWatching := w.watches.byPath(name)
	if !alreadyWatching {
		fi, err := os.Lstat(name)
		if err != nil {
			return "", err
		}

		// Don't watch sockets or named pipes.
		if (fi.Mode()&os.ModeSocket == os.ModeSocket) || (fi.Mode()&os.ModeNamedPipe == os.ModeNamedPipe) {
			return "", nil
		}

		// Follow symlinks, but only for paths added with Add(), and not paths
		// we're adding from internalWatch from a listdir.
		if !listDir && fi.Mode()&os.ModeSymlink == os.ModeSymlink {
			link, err := os.Readlink(name)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(name), link)
			}

			_, alreadyWatching = w.watches.byPath(link)
			if alreadyWatching {
				w.watches.addLink(name)
				return link, nil
			}

			info.linkName = name
			name = link
			fi, err = os.Lstat(name)
			if err != nil {
				return "", err
			}
		}

		info.wd, err = ignoringEINTR(func() (int, error) {
			return unix.Open(name, openMode, 0)
		})
		if err != nil {
			return "", err
		}
		info.isDir = fi.IsDir()
	}

	err := w.register([]int{info.wd}, unix.EV_ADD|unix.EV_CLEAR|unix.EV_ENABLE, flags)
	if err != nil {
		unix.Close(info.wd)
		return "", err
	}

	if !alreadyWatching {
		w.watches.add(name, info.linkName, info.wd, info.isDir)
	}

	if info.isDir {
		watchDir := (flags&unix.NOTE_WRITE) == unix.NOTE_WRITE &&
			(!alreadyWatching || (info.dirFlags&unix.NOTE_WRITE) != unix.NOTE_WRITE)
		if !w.watches.updateDirFlags(name, flags) {
			return "", nil
		}

		if watchDir {
			d := name
			if info.linkName != "" {
				d = info.linkName
			}
			if err := w.watchDirectoryFiles(d); err != nil {
				return "", err
			}
		}
	}
	return name, nil
}

// readEvents reads from kqueue and converts the received kevents into
// fsEvent values that it sends down the events channel.
func (w *kqueueWatcher) readEvents() {
	defer func() {
		close(w.events)
		close(w.errors)
		_ = unix.Close(w.kq)
		unix.Close(w.closepipe[0])
	}()

	eventBuffer := make([]unix.Kevent_t, 10)
	for {
		kevents, err := ignoringEINTR(func() ([]unix.Kevent_t, error) {
			return w.read(eventBuffer)
		})
		if err != nil {
			if !w.sendError(fmt.Errorf("kqueue readEvents: %w", err)) {
				return
			}
		}

		for _, kevent := range kevents {
			var (
				wd   = int(kevent.Ident)
				mask = uint32(kevent.Fflags)
			)

			// Shut down the loop when the pipe is closed, but only after all
			// other events have been processed.
			if wd == w.closepipe[0] {
				return
			}

			path, ok := w.watches.byWd(wd)

			// On macOS, sometimes an event with Ident=0 is delivered even
			// though we never saw such a file descriptor. Skip it if there's
			// no matching path.
			if !ok && kevent.Ident == 0 && runtime.GOOS == "darwin" {
				continue
			}

			event := w.newEvent(path.name, path.linkName, mask)

			if event.Has(fsRename) || event.Has(fsRemove) {
				w.remove(event.Name, false)
				w.watches.markSeen(event.Name, false)
			}

			if path.isDir && event.Has(fsWrite) && !event.Has(fsRemove) {
				w.dirChange(event.Name)
			} else if !w.sendEvent(event) {
				return
			}

			if event.Has(fsRemove) {
				if path.isDir {
					fileDir := filepath.Clean(event.Name)
					_, found := w.watches.byPath(fileDir)
					if found {
						err := w.dirChange(fileDir)
						if !w.sendError(err) {
							return
						}
					}
				} else {
					p := filepath.Clean(event.Name)
					if fi, err := os.Lstat(p); err == nil {
						err := w.sendCreateIfNew(p, fi)
						if !w.sendError(err) {
							return
						}
					}
				}
			}
		}
	}
}

// newEvent returns an fsEvent based on kqueue Fflags.
func (w *kqueueWatcher) newEvent(name, linkName string, mask uint32) fsEvent {
	e := fsEvent{Name: name}
	if linkName != "" {
		e.Name = linkName
	}

	if mask&unix.NOTE_DELETE == unix.NOTE_DELETE {
		e.Op |= fsRemove
	}
	if mask&unix.NOTE_WRITE == unix.NOTE_WRITE {
		e.Op |= fsWrite
	}
	if mask&unix.NOTE_RENAME == unix.NOTE_RENAME {
		e.Op |= fsRename
	}
	if mask&unix.NOTE_ATTRIB == unix.NOTE_ATTRIB {
		e.Op |= fsChmod
	}
	// No point sending a write and delete event at the same time: if it's
	// gone, then it's gone.
	if e.Op.Has(fsWrite) && e.Op.Has(fsRemove) {
		e.Op &^= fsWrite
	}
	return e
}

// watchDirectoryFiles opens an fd for every file in a directory, mimicking
// inotify's behavior of reporting events for children of watched directories.
func (w *kqueueWatcher) watchDirectoryFiles(dirPath string) error {
	files, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}

	for _, f := range files {
		path := filepath.Join(dirPath, f.Name())

		fi, err := f.Info()
		if err != nil {
			return fmt.Errorf("%q: %w", path, err)
		}

		cleanPath, err := w.internalWatch(path, fi)
		if err != nil {
			// No permission, or the entry resolved to a missing target
			// (e.g. a dangling symlink): not a problem, just skip. But
			// do mark it as seen to prevent it from being picked up as
			// a "new" file later (it still shows up in the directory
			// listing).
			switch {
			case errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) ||
				errors.Is(err, os.ErrNotExist):
				cleanPath = filepath.Clean(path)
			default:
				return fmt.Errorf("%q: %w", path, err)
			}
		}

		w.watches.markSeen(cleanPath, true)
	}

	return nil
}

// dirChange searches the directory for new files and sends an event for them.
func (w *kqueueWatcher) dirChange(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("kqueue dirChange %q: %w", dir, err)
	}

	for _, f := range files {
		fi, err := f.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("kqueue dirChange: %w", err)
		}

		err = w.sendCreateIfNew(filepath.Join(dir, fi.Name()), fi)
		if err != nil {
			if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("kqueue dirChange: %w", err)
		}
	}
	return nil
}

// sendCreateIfNew sends a create event if the file isn't already being
// tracked, and starts watching this file.
func (w *kqueueWatcher) sendCreateIfNew(path string, fi os.FileInfo) error {
	if !w.watches.seenBefore(path) {
		if !w.sendEvent(fsEvent{Name: path, Op: fsCreate}) {
			return nil
		}
	}

	path, err := w.internalWatch(path, fi)
	if err != nil {
		return err
	}
	w.watches.markSeen(path, true)
	return nil
}

func (w *kqueueWatcher) internalWatch(name string, fi os.FileInfo) (string, error) {
	if fi.IsDir() {
		info, _ := w.watches.byPath(name)
		return w.addWatch(name, info.dirFlags|unix.NOTE_DELETE|unix.NOTE_RENAME, true)
	}

	return w.addWatch(name, noteAllEvents, true)
}

// register submits kevent changes to the kqueue for the given file descriptors.
func (w *kqueueWatcher) register(fds []int, flags int, fflags uint32) error {
	changes := make([]unix.Kevent_t, len(fds))
	for i, fd := range fds {
		unix.SetKevent(&changes[i], fd, unix.EVFILT_VNODE, flags)
		changes[i].Fflags = fflags
	}

	success, err := unix.Kevent(w.kq, changes, nil, nil)
	if success == -1 {
		return err
	}
	return nil
}

// read retrieves pending events, or waits until an event occurs.
func (w *kqueueWatcher) read(events []unix.Kevent_t) ([]unix.Kevent_t, error) {
	n, err := unix.Kevent(w.kq, nil, events, nil)
	if err != nil {
		return nil, err
	}
	return events[0:n], nil
}

// ignoringEINTR makes a function call and repeats it if it returns an EINTR
// error. This is required even though signal handlers use SA_RESTART.
func ignoringEINTR[T any](fn func() (T, error)) (T, error) {
	for {
		v, err := fn()
		if err != unix.EINTR {
			return v, err
		}
	}
}
