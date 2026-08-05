//go:build !(linux && amd64)

package main

import (
	"fmt"
	"runtime"
)

// installCUDAPlugin reports that CUDA is unavailable on this platform.
//
// NVIDIA publishes the PJRT CUDA plugin for linux/amd64 only, and go-xla's
// installer for it is behind the same build constraint. Failing with a clear
// message beats a confusing download error — or worse, silently installing
// nothing and falling back to a 12x slower backend.
func installCUDAPlugin(string) error {
	return fmt.Errorf("the CUDA PJRT plugin is only published for linux/amd64, not %s/%s; "+
		"use --xla for the CPU plugin instead", runtime.GOOS, runtime.GOARCH)
}
