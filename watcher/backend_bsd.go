//go:build freebsd || openbsd || netbsd || dragonfly

package watcher

import "golang.org/x/sys/unix"

// openMode is the flag used when opening file descriptors for kqueue
// monitoring. On BSD systems, O_RDONLY is used since O_EVTONLY is
// macOS-specific.
const openMode = unix.O_NONBLOCK | unix.O_RDONLY | unix.O_CLOEXEC
