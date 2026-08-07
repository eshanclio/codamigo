package localembed_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ieshan/codamigo/localembed"
)

const testHash = "0123456789abcdef0123456789abcdef01234567"

func TestPin_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := localembed.Pin{
		RepoID:       "org/model",
		CommitHash:   testHash,
		ResolvedFrom: "main",
		ResolvedAt:   time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC),
		Dimensions:   768,
		RepoInfo:     json.RawMessage(`{"sha":"` + testHash + `","siblings":[]}`),
	}
	if err := localembed.WritePin(dir, want); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	got, err := localembed.ReadPin(dir)
	if err != nil {
		t.Fatalf("ReadPin: %v", err)
	}
	if got.CommitHash != want.CommitHash {
		t.Errorf("CommitHash = %q, want %q", got.CommitHash, want.CommitHash)
	}
	if got.RepoID != want.RepoID {
		t.Errorf("RepoID = %q, want %q", got.RepoID, want.RepoID)
	}
	if got.ResolvedFrom != want.ResolvedFrom {
		t.Errorf("ResolvedFrom = %q, want %q", got.ResolvedFrom, want.ResolvedFrom)
	}
	if got.Dimensions != want.Dimensions {
		t.Errorf("Dimensions = %d, want %d", got.Dimensions, want.Dimensions)
	}
	if !got.ResolvedAt.Equal(want.ResolvedAt) {
		t.Errorf("ResolvedAt = %v, want %v", got.ResolvedAt, want.ResolvedAt)
	}
	// RepoInfo must survive byte-for-byte: the shim serves it verbatim.
	if string(got.RepoInfo) != string(want.RepoInfo) {
		t.Errorf("RepoInfo = %s, want %s", got.RepoInfo, want.RepoInfo)
	}
}

func TestPin_WriteUsesRestrictivePermissions(t *testing.T) {
	dir := t.TempDir()
	p := localembed.Pin{RepoID: "org/model", CommitHash: testHash, RepoInfo: json.RawMessage(`{}`)}
	if err := localembed.WritePin(dir, p); err != nil {
		t.Fatalf("WritePin: %v", err)
	}
	info, err := os.Stat(localembed.PinPath(dir))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestPin_ReadAbsentIsErrNoPin(t *testing.T) {
	if _, err := localembed.ReadPin(t.TempDir()); !errors.Is(err, localembed.ErrNoPin) {
		t.Errorf("ReadPin on empty dir = %v, want ErrNoPin", err)
	}
}

func TestPin_ReadCorruptIsErrNoPin(t *testing.T) {
	// A truncated or garbage pin file must read as "no pin" so callers fall
	// through to derivation rather than failing outright.
	for name, body := range map[string]string{
		"not json":     "{{{",
		"empty object": "{}",
		"short hash":   `{"repo_id":"org/model","commit_hash":"abc","repo_info":{}}`,
		"non-hex hash": `{"repo_id":"org/model","commit_hash":"zzzz56789abcdef0123456789abcdef01234567","repo_info":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "codamigo-pin.json"), []byte(body), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := localembed.ReadPin(dir); !errors.Is(err, localembed.ErrNoPin) {
				t.Errorf("ReadPin = %v, want ErrNoPin", err)
			}
		})
	}
}
