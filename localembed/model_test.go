package localembed_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ieshan/codamigo/localembed"
)

func TestLookup_RegistryShortName(t *testing.T) {
	m, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.RepoID != "BAAI/bge-small-en-v1.5" {
		t.Errorf("RepoID = %q, want BAAI/bge-small-en-v1.5", m.RepoID)
	}
	if m.Dimensions != 384 {
		t.Errorf("Dimensions = %d, want 384", m.Dimensions)
	}
	if !m.Registered {
		t.Error("Registered = false, want true for a registry model")
	}
	if !m.Pinned() {
		t.Error("Pinned() = false, want true: every registry entry must carry checksums")
	}
	if m.QueryPrefix == "" {
		t.Error("QueryPrefix is empty; bge is asymmetric and needs its query instruction")
	}
}

func TestLookup_RegistryRepoID(t *testing.T) {
	// The full repo id of a registry model must resolve to the pinned entry, not
	// to an unpinned passthrough.
	m, err := localembed.Lookup("BAAI/bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !m.Registered || !m.Pinned() {
		t.Errorf("Registered = %v, Pinned = %v; want both true", m.Registered, m.Pinned())
	}
	if m.Name != "bge-small-en-v1.5" {
		t.Errorf("Name = %q, want bge-small-en-v1.5", m.Name)
	}
}

func TestLookup_EmptyUsesDefault(t *testing.T) {
	m, err := localembed.Lookup("")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.Name != localembed.DefaultModel {
		t.Errorf("Name = %q, want %q", m.Name, localembed.DefaultModel)
	}
}

func TestLookup_UnknownBareName(t *testing.T) {
	// A typo must fail locally rather than after a network round trip.
	_, err := localembed.Lookup("bge-small")
	if err == nil {
		t.Fatal("Lookup(bge-small) = nil error, want error")
	}
	if !errors.Is(err, localembed.ErrUnknownModel) {
		t.Errorf("error should wrap ErrUnknownModel, got: %v", err)
	}
	if !strings.Contains(err.Error(), "bge-small-en-v1.5") {
		t.Errorf("error should list the known models, got: %v", err)
	}
}

func TestLookup_UnpinnedRepoID(t *testing.T) {
	m, err := localembed.Lookup("some-org/some-model")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if m.Registered {
		t.Error("Registered = true, want false for a raw repository id")
	}
	if m.Pinned() {
		t.Error("Pinned() = true; a raw repository id has no checksums to verify against")
	}
	if m.Revision != "main" {
		t.Errorf("Revision = %q, want main", m.Revision)
	}
	if m.Dimensions != 0 {
		t.Errorf("Dimensions = %d, want 0 so the caller must declare it", m.Dimensions)
	}
	if len(m.Files) == 0 {
		t.Error("Files is empty; an unpinned model still needs the standard manifest")
	}
	for _, f := range m.Files {
		if f.SHA256 != "" || f.Size != 0 {
			t.Errorf("file %s has pinned metadata %d/%q on an unpinned model", f.Path, f.Size, f.SHA256)
		}
	}
}

func TestLookup_RejectsMalformedRepoID(t *testing.T) {
	for _, id := range []string{
		"owner/",
		"/name",
		"a/b/c",
		"../evil/model",
		"owner/../../etc",
		"owner/na me",
		"owner/na\x00me",
		"owner/..",
		"./x",
	} {
		t.Run(id, func(t *testing.T) {
			if _, err := localembed.Lookup(id); err == nil {
				t.Errorf("Lookup(%q) = nil error, want rejection", id)
			}
		})
	}
}

func TestLookup_DoesNotAliasRegistry(t *testing.T) {
	// Callers get their own copy: mutating a returned manifest must not poison
	// the registry for the rest of the process.
	a, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	a.Files[0].SHA256 = "tampered"
	b, err := localembed.Lookup("bge-small-en-v1.5")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if b.Files[0].SHA256 == "tampered" {
		t.Error("mutating a looked-up manifest changed the registry")
	}
}

func TestModel_DirName(t *testing.T) {
	tests := []struct {
		name  string
		model localembed.Model
		want  string
	}{
		{"registry short name", localembed.Model{Name: "bge-small-en-v1.5", RepoID: "BAAI/bge-small-en-v1.5"}, "bge-small-en-v1.5"},
		{"repo id flattened", localembed.Model{RepoID: "some-org/some-model"}, "some-org_some-model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.model.DirName(); got != tt.want {
				t.Errorf("DirName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModel_DisplayName(t *testing.T) {
	if got := (localembed.Model{Name: "short", RepoID: "org/long"}).DisplayName(); got != "short" {
		t.Errorf("DisplayName() = %q, want short", got)
	}
	if got := (localembed.Model{RepoID: "org/long"}).DisplayName(); got != "org/long" {
		t.Errorf("DisplayName() = %q, want org/long", got)
	}
}

func TestRegistryNames_IncludesDefault(t *testing.T) {
	names := localembed.RegistryNames()
	found := false
	for _, n := range names {
		if n == localembed.DefaultModel {
			found = true
		}
	}
	if !found {
		t.Errorf("RegistryNames() = %v, missing the default model %q", names, localembed.DefaultModel)
	}
}
