package localembed

import (
	"fmt"
	"slices"
	"testing"
)

// bucketize is the highest-value unit in this package: a bug silently mispairs
// vectors with texts, and the closed-shape-set assertion is what the memory
// design rests on.

func TestSeqBuckets(t *testing.T) {
	tests := []struct {
		maxSeqLen int
		want      []int
	}{
		{8, []int{8}},
		{16, []int{8, 16}},
		{128, []int{8, 16, 32, 64, 128}},
		{512, []int{8, 16, 32, 64, 128, 256, 512}},
		// Not a power of two: the last entry is the requested maximum, so nothing
		// is truncated more aggressively than asked.
		{100, []int{8, 16, 32, 64, 100}},
		{4, []int{4}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.maxSeqLen), func(t *testing.T) {
			got, err := seqBuckets(tt.maxSeqLen)
			if err != nil {
				t.Fatalf("seqBuckets(%d): %v", tt.maxSeqLen, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("seqBuckets(%d) = %v, want %v", tt.maxSeqLen, got, tt.want)
			}
		})
	}
	if _, err := seqBuckets(0); err == nil {
		t.Error("seqBuckets(0) = nil error, want error")
	}
}

func TestBatchBuckets(t *testing.T) {
	tests := []struct {
		maxBatch int
		want     []int
	}{
		{32, []int{1, 8, 32}},
		{16, []int{1, 8, 16}},
		{8, []int{1, 8}},
		{4, []int{1, 4}},
		{1, []int{1}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.maxBatch), func(t *testing.T) {
			got, err := batchBuckets(tt.maxBatch)
			if err != nil {
				t.Fatalf("batchBuckets(%d): %v", tt.maxBatch, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("batchBuckets(%d) = %v, want %v", tt.maxBatch, got, tt.want)
			}
		})
	}
	if _, err := batchBuckets(0); err == nil {
		t.Error("batchBuckets(0) = nil error, want error")
	}
}

func TestBucketize_Grouping(t *testing.T) {
	batchB := []int{1, 8, 32}
	seqB := []int{8, 16, 32, 64, 128, 256, 512}

	tests := []struct {
		name    string
		lengths []int
		want    []batch
	}{
		{
			name:    "empty input",
			lengths: nil,
			want:    nil,
		},
		{
			name:    "single short text takes the batch-of-one shape",
			lengths: []int{5},
			want:    []batch{{indices: []int{0}, rows: 1, seqLen: 8}},
		},
		{
			name:    "lengths round up to the next seq bucket",
			lengths: []int{9, 17},
			want: []batch{
				{indices: []int{0}, rows: 1, seqLen: 16},
				{indices: []int{1}, rows: 1, seqLen: 32},
			},
		},
		{
			name:    "same bucket groups together and keeps input order",
			lengths: []int{10, 12, 16},
			want:    []batch{{indices: []int{0, 1, 2}, rows: 8, seqLen: 16}},
		},
		{
			name:    "full batch then a padded remainder",
			lengths: repeat(40, 20),
			want: []batch{
				{indices: seq(0, 32), rows: 32, seqLen: 32},
				// The tail rounds up to 8 rather than splitting into ones: a few
				// wasted rows beat several extra graph invocations.
				{indices: seq(32, 40), rows: 8, seqLen: 32},
			},
		},
		{
			name:    "remainder of one",
			lengths: repeat(33, 20),
			want: []batch{
				{indices: seq(0, 32), rows: 32, seqLen: 32},
				{indices: seq(32, 33), rows: 1, seqLen: 32},
			},
		},
		{
			name:    "over-length inputs land in the largest bucket for truncation",
			lengths: []int{600, 1000},
			want:    []batch{{indices: []int{0, 1}, rows: 8, seqLen: 512}},
		},
		{
			name:    "buckets are emitted in ascending order regardless of input order",
			lengths: []int{200, 5, 100},
			want: []batch{
				{indices: []int{1}, rows: 1, seqLen: 8},
				{indices: []int{2}, rows: 1, seqLen: 128},
				{indices: []int{0}, rows: 1, seqLen: 256},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bucketize(tt.lengths, batchB, seqB)
			if err != nil {
				t.Fatalf("bucketize: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("bucketize returned %d batches, want %d: got %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i].rows != tt.want[i].rows || got[i].seqLen != tt.want[i].seqLen ||
					!slices.Equal(got[i].indices, tt.want[i].indices) {
					t.Errorf("batch %d = {indices:%v rows:%d seqLen:%d}, want {indices:%v rows:%d seqLen:%d}",
						i, got[i].indices, got[i].rows, got[i].seqLen,
						tt.want[i].indices, tt.want[i].rows, tt.want[i].seqLen)
				}
			}
		})
	}
}

// TestBucketize_Invariants asserts the three properties the rest of the package
// relies on, across a spread of sizes including the awkward ones around bucket
// boundaries.
func TestBucketize_Invariants(t *testing.T) {
	batchB := []int{1, 8, 32}
	seqB := []int{8, 16, 32, 64, 128, 256, 512}

	// The closed set the graph cache is sized for.
	closed := make(map[[2]int]bool, len(batchB)*len(seqB))
	for _, b := range batchB {
		for _, s := range seqB {
			closed[[2]int{b, s}] = true
		}
	}

	for _, n := range []int{1, 2, 7, 8, 9, 31, 32, 33, 40, 63, 64, 100, 257, 1000} {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			// A spread of lengths so many seq buckets are exercised at once.
			lengths := make([]int, n)
			for i := range lengths {
				lengths[i] = 1 + (i*37)%700 // deliberately crosses the 512 cap
			}
			batches, err := bucketize(lengths, batchB, seqB)
			if err != nil {
				t.Fatalf("bucketize: %v", err)
			}

			seen := make([]int, n)
			for _, b := range batches {
				if !closed[[2]int{b.rows, b.seqLen}] {
					t.Errorf("shape [%d, %d] is outside the precompiled set", b.rows, b.seqLen)
				}
				if b.rows < len(b.indices) {
					t.Errorf("batch has %d indices but only %d rows", len(b.indices), b.rows)
				}
				if len(b.indices) == 0 {
					t.Error("batch has no indices")
				}
				for _, i := range b.indices {
					if i < 0 || i >= n {
						t.Fatalf("index %d out of range [0,%d)", i, n)
					}
					seen[i]++
				}
			}
			for i, count := range seen {
				if count != 1 {
					t.Errorf("index %d appears in %d batches, want exactly 1", i, count)
				}
			}
		})
	}
}

// TestBucketize_IndicesDoNotAlias guards the disjoint-write assumption in
// EmbedBatchPartial: each batch's index slice is capped, so appending to one
// cannot reach into another's backing array.
func TestBucketize_IndicesDoNotAlias(t *testing.T) {
	batches, err := bucketize(repeat(40, 20), []int{1, 8, 32}, []int{8, 16, 32})
	if err != nil {
		t.Fatalf("bucketize: %v", err)
	}
	if len(batches) < 2 {
		t.Fatalf("expected at least 2 batches, got %d", len(batches))
	}
	first := batches[0].indices
	before := slices.Clone(batches[1].indices)
	_ = append(first, -1) //nolint:gocritic // deliberately probing for aliasing
	if !slices.Equal(batches[1].indices, before) {
		t.Errorf("appending to batch 0's indices modified batch 1: %v, want %v", batches[1].indices, before)
	}
}

func TestBucketize_EmptyLadders(t *testing.T) {
	if _, err := bucketize([]int{1}, nil, []int{8}); err == nil {
		t.Error("bucketize with no batch ladder = nil error, want error")
	}
	if _, err := bucketize([]int{1}, []int{1}, nil); err == nil {
		t.Error("bucketize with no seq ladder = nil error, want error")
	}
}

func repeat(n, value int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = value
	}
	return out
}

func seq(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}
