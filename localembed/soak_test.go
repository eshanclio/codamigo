package localembed_test

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestSoak_NoLeak drives many batches across the whole closed shape set and
// checks the Go heap returns to its baseline afterwards, which is the signature a
// missed FinalizeAll would break. Under XLA the same bug exhausts VRAM instead of
// the heap, so this runs on whichever backend is selected.
//
// Skipped unless CODAMIGO_SOAK=1: it takes tens of seconds, so it is a diagnostic
// rather than a gate.
func TestSoak_NoLeak(t *testing.T) {
	if os.Getenv("CODAMIGO_SOAK") != "1" {
		t.Skip("set CODAMIGO_SOAK=1 to run the leak soak")
	}
	e := newTestEmbedder(t, "bge-small-en-v1.5", "auto")

	// Cross every seq bucket and every batch bucket.
	batches := make([][]string, 0, 24)
	for _, n := range []int{1, 5, 20, 40} {
		for _, words := range []int{2, 10, 40, 150, 400} {
			texts := make([]string, n)
			for i := range texts {
				texts[i] = strings.Repeat("token "+strconv.Itoa(i)+" ", words)
			}
			batches = append(batches, texts)
		}
	}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	t.Logf("baseline: HeapAlloc=%.1f MiB", mib(base.HeapAlloc))

	rounds := 4
	if n, err := strconv.Atoi(os.Getenv("CODAMIGO_SOAK_ROUNDS")); err == nil && n > 0 {
		rounds = n
	}
	for round := range rounds {
		for _, texts := range batches {
			_, errs := e.EmbedBatchPartial(t.Context(), texts)
			for i, err := range errs {
				if err != nil {
					t.Fatalf("round %d index %d: %v", round, i, err)
				}
			}
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		t.Logf("round %d: HeapAlloc=%.1f MiB", round, mib(ms.HeapAlloc))
	}

	for range 3 {
		runtime.GC()
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("after GC: HeapAlloc=%.1f MiB", mib(after.HeapAlloc))

	// runtime.Sys is deliberately not used here: it is reserved-not-returned and
	// monotonic by design, so it reports growth even when nothing leaks.
	growth := mib(after.HeapAlloc) - mib(base.HeapAlloc)
	if growth > 128 {
		t.Errorf("heap grew %.1f MiB across %d rounds after GC; suspect a missing FinalizeAll",
			growth, rounds)
	}
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }
