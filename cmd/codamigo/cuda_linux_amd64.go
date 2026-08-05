//go:build linux && amd64

package main

import (
	"fmt"
	"path/filepath"

	"github.com/gomlx/go-xla/installer"
)

// installCUDAPlugin installs the CUDA PJRT plugin for a specific CUDA major
// version.
//
// This exists separately from --xla because AutoInstall's CUDA path self-gates on
// a visible NVIDIA GPU, which makes it a no-op during a container build. Naming
// the version explicitly installs the plugin regardless, which is how
// Dockerfile.cuda bakes it in.
//
// go-xla's installer/cuda.go is itself behind a `(linux && amd64) || pjrt_all`
// build constraint, so CudaInstall does not exist on other platforms — hence the
// file split rather than a runtime check.
func installCUDAPlugin(cudaVersion string) error {
	plugin, ok := map[string]string{"12": "cuda12", "13": "cuda13"}[cudaVersion]
	if !ok {
		return fmt.Errorf("--cuda must be 12 or 13, got %q", cudaVersion)
	}
	libPath, err := installer.DefaultHomeLibPath()
	if err != nil {
		return fmt.Errorf("resolving the plugin install directory: %w", err)
	}
	// CudaInstall expects the ".../lib/go-xla" directory; it creates the nvidia/
	// subdirectory underneath itself.
	installPath := filepath.Join(libPath, "go-xla")
	fmt.Printf("Installing the %s PJRT plugin into %s\n", plugin, installPath)
	return installer.CudaInstall(plugin, "latest", installPath, true, installer.Normal)
}
