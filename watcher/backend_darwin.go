//go:build darwin

package watcher

import "golang.org/x/sys/unix"

// openMode is the flag used when opening file descriptors for kqueue
// monitoring. On macOS, O_EVTONLY opens the file for event notification
// only, without granting read access.
const openMode = unix.O_EVTONLY | unix.O_CLOEXEC
