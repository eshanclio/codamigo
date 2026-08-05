package localembed

import (
	"strings"
	"testing"
)

// TestRegistry_EveryEntryIsPinned is the invariant that makes Download a
// supply-chain check rather than a corruption check: an entry with a missing or
// invented checksum would either skip verification silently or reject a
// legitimate file.
//
// It reads the registry directly rather than through an exported accessor —
// this asserts a property of the package's own data, and exporting a getter
// only tests would call would be shipping API for a test's benefit.
func TestRegistry_EveryEntryIsPinned(t *testing.T) {
	if len(registry) == 0 {
		t.Fatal("registry is empty")
	}
	for _, m := range registry {
		t.Run(m.Name, func(t *testing.T) {
			if m.Name == "" {
				t.Error("registry entry has no short name")
			}
			if !strings.Contains(m.RepoID, "/") {
				t.Errorf("RepoID %q is not an owner/name repository id", m.RepoID)
			}
			if len(m.Revision) != 40 {
				t.Errorf("Revision %q is not a 40-character commit hash; a moving "+
					"revision cannot be checksum-pinned", m.Revision)
			}
			if m.Dimensions <= 0 {
				t.Errorf("Dimensions = %d, want positive", m.Dimensions)
			}
			if !m.Pinned() {
				t.Error("entry is not fully pinned")
			}
			for _, f := range m.Files {
				if len(f.SHA256) != 64 {
					t.Errorf("%s: SHA256 %q is not 64 hex characters", f.Path, f.SHA256)
				}
				if f.Size <= 0 {
					t.Errorf("%s: Size = %d, want positive", f.Path, f.Size)
				}
			}
		})
	}
}
