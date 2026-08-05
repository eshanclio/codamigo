//go:build race

package localembed_test

// raceEnabled reports whether the test binary was built with -race.
//
// It exists to skip inference on the pure-Go compute backend under the race
// detector. -race also turns on checkptr, which fatals inside GoMLX's own
// hand-optimized matmul kernel (compute/internal/gobackend/dot/matmul:
// "checkptr: pointer arithmetic result points to invalid allocation"). That is
// upstream unsafe code, unrelated to anything this package does, and it aborts
// the whole test binary rather than failing one test. The XLA backend does its
// matmuls in PJRT, so it is unaffected and still covers the concurrency paths.
const raceEnabled = true
