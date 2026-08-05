package localembed

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/gomlx/compute"
	"github.com/gomlx/compute/dtypes"

	// Registers the compiled-in backends ("go", "xla", ...) with compute.
	_ "github.com/gomlx/gomlx/backends/default"
)

// Measured on this repo's reference machine (darwin/arm64, bge-small, batch 8,
// steady state), in embeddings per second. Reproduce with BenchmarkEmbedBatch:
//
//	seqLen   go     xla   speedup
//	    32   7.5   186.6      25x
//	   128   3.5    51.8      15x
//	   256   1.5    18.2      12x
//	   512   0.5     7.0      15x
//
// XLA-CPU is 12–25x faster, which is the difference between a usable local
// provider and an unusable one — indexing a few thousand chunks at 3.5/sec takes
// upwards of ten minutes. So "auto" tries XLA first and treats the pure-Go
// backend as a correctness fallback, not a default.

// backendConfigs returns the ordered candidate list for a configured backend
// value. Splitting this out from construction keeps the fallback order testable
// without a working compute backend.
//
// "auto" prefers CUDA, then XLA-CPU, then pure Go. An explicit value yields
// exactly itself: asking for xla:cuda and silently getting a 12-25x slower backend
// would be worse than an error.
func backendConfigs(cfg string) ([]string, error) {
	switch cfg {
	case "", "auto":
		return []string{"xla:cuda", "xla", "go"}, nil
	case "go", "xla", "xla:cpu", "xla:cuda":
		return []string{cfg}, nil
	default:
		return nil, fmt.Errorf("%w %q (use auto, go, xla, xla:cpu, or xla:cuda)", ErrUnsupportedBackend, cfg)
	}
}

// noAutoInstall is applied once per process. GoMLX otherwise downloads a missing
// PJRT plugin on first use, which cost 5.6s and an unannounced network fetch in
// testing — unacceptable in the middle of an index run. With it set, a missing
// plugin fails immediately and "auto" falls through to the next candidate.
var noAutoInstall sync.Once

func disableAutoInstall() {
	noAutoInstall.Do(func() {
		if os.Getenv("GOMLX_NO_AUTO_INSTALL") == "" {
			// Deliberately process-global: this is the only knob GoMLX exposes.
			// Set before any backend is constructed, and never cleared.
			_ = os.Setenv("GOMLX_NO_AUTO_INSTALL", "1")
		}
	})
}

// selectBackend constructs the first backend in the candidate list that works,
// returning it along with the config string that produced it.
//
// compute.List reports the backends compiled in, not the ones that can actually
// run — a PJRT plugin may still be missing — so candidates are tried rather than
// filtered.
func selectBackend(cfg string) (compute.Backend, string, error) {
	candidates, err := backendConfigs(cfg)
	if err != nil {
		return nil, "", err
	}
	disableAutoInstall()

	var attempts []error
	for _, candidate := range candidates {
		backend, err := compute.NewWithConfig(candidate)
		if err != nil {
			attempts = append(attempts, fmt.Errorf("%s: %w", candidate, err))
			slog.Debug("local embedding backend unavailable",
				slog.String("backend", candidate), slog.Any("error", err))
			continue
		}
		if err := checkCapabilities(backend); err != nil {
			backend.Finalize()
			attempts = append(attempts, fmt.Errorf("%s: %w", candidate, err))
			continue
		}
		slog.Info("local embedding backend selected",
			slog.String("backend", candidate), slog.String("name", backend.Name()))
		return backend, candidate, nil
	}
	return nil, "", fmt.Errorf("%w: no usable backend among %v: %w",
		ErrUnsupportedBackend, candidates, errors.Join(attempts...))
}

// checkCapabilities rejects a backend that cannot run float32, so the failure
// surfaces at construction rather than partway through an index run.
func checkCapabilities(backend compute.Backend) error {
	caps := backend.Capabilities()
	if len(caps.DTypes) > 0 && !caps.DTypes[dtypes.Float32] {
		return fmt.Errorf("backend %q does not support float32", backend.Name())
	}
	return nil
}
