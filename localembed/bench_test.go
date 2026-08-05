package localembed_test

import (
	"strconv"
	"strings"
	"testing"
)

// BenchmarkEmbedBatch reproduces the throughput table in README.md and in the
// comment at the top of backend.go. Those numbers are the whole justification
// for preferring XLA over the pure-Go backend in "auto", and for calling `go` a
// correctness fallback rather than a default — so they need to be reproducible
// rather than remembered.
//
//	make bench-localembed
//
// Skips when the model is absent, like the rest of the inference suite. The
// pure-Go rows are the slow ones by design: at seq 512 one iteration takes
// seconds, which is exactly the finding being recorded.
func BenchmarkEmbedBatch(b *testing.B) {
	const batch = 8
	for _, backend := range []string{"go", "xla"} {
		for _, seqLen := range []int{32, 128, 256, 512} {
			b.Run(backend+"/seq"+strconv.Itoa(seqLen), func(b *testing.B) {
				e := newTestEmbedder(b, "bge-small-en-v1.5", backend)
				// seqLen-2 repetitions, not seqLen: "token" is one token in this
				// vocabulary and the tokenizer adds CLS and SEP, so seqLen
				// repetitions would land in the *next* bucket up and mislabel the
				// row. seqBuckets contains exactly these powers of two, so this
				// hits the bucket named in the sub-benchmark.
				texts := make([]string, batch)
				for i := range texts {
					texts[i] = strings.Repeat("token ", seqLen-2)
				}

				// Compile this shape before timing. Compilation is once per shape
				// per process, so amortizing it into a throughput figure would
				// understate steady-state indexing by roughly half.
				if _, err := e.EmbedBatch(b.Context(), texts); err != nil {
					b.Fatalf("warmup: %v", err)
				}

				b.ResetTimer()
				for b.Loop() {
					vectors, errs := e.EmbedBatchPartial(b.Context(), texts)
					for i := range texts {
						if errs[i] != nil {
							b.Fatalf("index %d: %v", i, errs[i])
						}
						if len(vectors[i]) != e.Dim() {
							b.Fatalf("index %d: %d dims, want %d", i, len(vectors[i]), e.Dim())
						}
					}
				}

				// The headline figure, directly comparable to the README table.
				b.ReportMetric(float64(batch*b.N)/b.Elapsed().Seconds(), "emb/s")
			})
		}
	}
}
