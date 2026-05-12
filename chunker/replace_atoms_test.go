package chunker

import (
	"testing"
)

// TestReplaceAtomsInRange verifies both the PF-15 early-return (no-match) path
// and the replacement path, including byte-order preservation.
func TestReplaceAtomsInRange(t *testing.T) {
	src := []byte("hello world")

	tests := []struct {
		name         string
		atoms        []atom
		start        int
		end          int
		replacements []atom
		wantLen      int
		// noMatch, when true, verifies that the original slice is returned unchanged.
		noMatch bool
		// wantPositions lists expected [startByte, endByte] pairs for each result atom.
		// Only checked when noMatch is false.
		wantPositions [][2]int
	}{
		{
			name: "no match returns original slice",
			atoms: []atom{
				{startByte: 0, endByte: 10},
				{startByte: 20, endByte: 30},
			},
			start: 100,
			end:   200,
			replacements: []atom{
				{startByte: 50, endByte: 60},
			},
			wantLen: 2,
			noMatch: true,
		},
		{
			name: "with match replaces and preserves order",
			atoms: []atom{
				{startByte: 0, endByte: 5, src: src},
				{startByte: 6, endByte: 11, src: src},
				{startByte: 12, endByte: 20, src: src},
			},
			start: 6,
			end:   11,
			replacements: []atom{
				{startByte: 6, endByte: 8, src: src},
				{startByte: 8, endByte: 11, src: src},
			},
			wantLen: 4,
			noMatch: false,
			wantPositions: [][2]int{
				{0, 5},
				{6, 8},
				{8, 11},
				{12, 20},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := replaceAtomsInRange(tc.atoms, tc.start, tc.end, tc.replacements)

			if len(result) != tc.wantLen {
				t.Fatalf("expected %d atoms, got %d", tc.wantLen, len(result))
			}

			if tc.noMatch {
				// PF-15: no allocation on the no-match path — original backing array must be returned.
				if &result[0] != &tc.atoms[0] {
					t.Error("expected original slice to be returned unchanged (no allocation), got a new slice")
				}
				return
			}

			// Verify positional correctness.
			for i, want := range tc.wantPositions {
				if result[i].startByte != want[0] || result[i].endByte != want[1] {
					t.Errorf("result[%d]: expected [%d,%d), got [%d,%d)",
						i, want[0], want[1], result[i].startByte, result[i].endByte)
				}
			}

			// Verify byte-order is monotonically non-decreasing across all result atoms.
			for i := 1; i < len(result); i++ {
				if result[i].startByte < result[i-1].startByte {
					t.Errorf("byte order violated: result[%d].startByte=%d < result[%d].startByte=%d",
						i, result[i].startByte, i-1, result[i-1].startByte)
				}
			}
		})
	}
}

// BenchmarkReplaceAtomsInRange_NoMatch measures that the no-match path is allocation-free.
func BenchmarkReplaceAtomsInRange_NoMatch(b *testing.B) {
	atoms := make([]atom, 100)
	for i := range atoms {
		atoms[i] = atom{startByte: i * 10, endByte: i*10 + 5}
	}
	replacements := []atom{{startByte: 2000, endByte: 2010}}

	b.ReportAllocs()
	for b.Loop() {
		replaceAtomsInRange(atoms, 5000, 6000, replacements)
	}
}
