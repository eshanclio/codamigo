//go:build !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !linux

package watcher

import "errors"

// newBackend returns an error on unsupported platforms.
func newBackend(ev chan fsEvent, errs chan error) (backend, error) {
	return nil, errors.New("watcher: kernel notifications not supported on this platform")
}
