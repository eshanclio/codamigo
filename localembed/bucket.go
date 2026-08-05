package localembed

import (
	"fmt"
	"slices"
)

// GoMLX compiles one executable per distinct input shape, so both axes of the
// [batch, seqLen] input are quantized to a closed set. Padding a short final
// batch up to a bucket matters more than it looks: without it every index run's
// tail produces a one-off shape, which is the most likely way the graph cache
// grows without bound.
//
// bucketize is a pure function rather than go-huggingface's streaming bucket.Run
// so that the closed-set invariant can be asserted directly in a test, and so
// there is no channel close/drain/cancel lifecycle to deadlock on.

// batch is one graph invocation: which of the caller's texts it covers, and the
// padded shape it runs at.
type batch struct {
	// indices are positions in the caller's texts slice, ascending.
	indices []int
	// rows is the padded batch size — a member of the batch bucket ladder, and
	// always >= len(indices). Surplus rows are filled with pad tokens and their
	// results discarded.
	rows int
	// seqLen is the padded sequence length, a member of the seq bucket ladder.
	seqLen int
}

// seqBuckets returns the sequence-length ladder for maxSeqLen: powers of two
// from 8 up to and including maxSeqLen. maxSeqLen itself is always the last
// entry even when it is not a power of two, so nothing is truncated further
// than the caller asked for.
func seqBuckets(maxSeqLen int) ([]int, error) {
	if maxSeqLen < 1 {
		return nil, fmt.Errorf("maxSeqLen must be positive, got %d", maxSeqLen)
	}
	var out []int
	for n := 8; n < maxSeqLen; n *= 2 {
		out = append(out, n)
	}
	return append(out, maxSeqLen), nil
}

// batchBuckets returns the batch-size ladder for maxBatch: 1, 8, maxBatch,
// dropping duplicates and any entry above maxBatch. 1 keeps a single-text query
// (the search path) from paying for a padded batch of 32; 8 softens the jump
// between them.
func batchBuckets(maxBatch int) ([]int, error) {
	if maxBatch < 1 {
		return nil, fmt.Errorf("maxBatch must be positive, got %d", maxBatch)
	}
	out := []int{1}
	for _, n := range []int{8, maxBatch} {
		if n <= maxBatch && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// bucketize groups texts of the given token lengths into batches whose shapes
// all come from batchBuckets × seqBuckets.
//
// Every index in [0, len(lengths)) appears in exactly one returned batch.
// Sequences longer than the largest seq bucket are assigned to it and truncated
// by the caller, matching sentence_transformers. Both ladders must be sorted
// ascending and non-empty.
func bucketize(lengths []int, batchB, seqB []int) ([]batch, error) {
	if len(batchB) == 0 || len(seqB) == 0 {
		return nil, fmt.Errorf("%w: bucket ladders must not be empty", ErrUnexpectedShape)
	}
	if len(lengths) == 0 {
		return nil, nil
	}
	maxSeq := seqB[len(seqB)-1]
	maxBatch := batchB[len(batchB)-1]

	// Group by seq bucket, preserving input order within each group so the
	// output is deterministic.
	groups := make(map[int][]int, len(seqB))
	for i, n := range lengths {
		bucket := maxSeq
		if n <= maxSeq {
			bucket = roundUp(n, seqB)
		}
		groups[bucket] = append(groups[bucket], i)
	}

	// Emit groups in ascending bucket order rather than map order.
	present := make([]int, 0, len(groups))
	for b := range groups {
		present = append(present, b)
	}
	slices.Sort(present)

	var out []batch
	for _, seqLen := range present {
		idx := groups[seqLen]
		for len(idx) > 0 {
			// Full batches while there are enough texts to fill one; then pad the
			// remainder up to the smallest bucket that holds it. Rounding the tail
			// up rather than splitting it into ones trades a few wasted rows for
			// far fewer graph invocations.
			take := maxBatch
			rows := maxBatch
			if len(idx) < maxBatch {
				take = len(idx)
				rows = roundUp(take, batchB)
			}
			out = append(out, batch{indices: idx[:take:take], rows: rows, seqLen: seqLen})
			idx = idx[take:]
		}
	}
	return out, nil
}

// roundUp returns the smallest bucket >= n, or the largest bucket when n
// exceeds all of them. buckets must be sorted ascending and non-empty.
func roundUp(n int, buckets []int) int {
	for _, b := range buckets {
		if n <= b {
			return b
		}
	}
	return buckets[len(buckets)-1]
}
