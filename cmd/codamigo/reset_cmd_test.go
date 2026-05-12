package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunReset_MissingStore(t *testing.T) {
	var out strings.Builder
	err := runReset("/nonexistent/store.db", false, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.String(), "Nothing to reset") {
		t.Errorf("expected 'Nothing to reset', got: %q", out.String())
	}
}

func TestRunReset_Force(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "store.db")
	if err := os.WriteFile(storePath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err := runReset(storePath, true, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(storePath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("store file should have been deleted")
	}
	if !strings.Contains(out.String(), "Store deleted") {
		t.Errorf("expected 'Store deleted', got: %q", out.String())
	}
}

func TestRunReset_ConfirmAbort(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "store.db")
	if err := os.WriteFile(storePath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err := runReset(storePath, false, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(storePath); statErr != nil {
		t.Error("store file should NOT have been deleted after abort")
	}
	if !strings.Contains(out.String(), "Aborted") {
		t.Errorf("expected 'Aborted', got: %q", out.String())
	}
}

func TestRunReset_ConfirmYes(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "store.db")
	if err := os.WriteFile(storePath, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	err := runReset(storePath, false, strings.NewReader("y\n"), &out)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(storePath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("store file should have been deleted after yes")
	}
}

func TestRunReset_DeletesWALSiblings(t *testing.T) {
	dir := t.TempDir()
	storePath := filepath.Join(dir, "test.db")

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.WriteFile(storePath+suffix, []byte("data"), 0o644); err != nil {
			t.Fatalf("creating %s: %v", storePath+suffix, err)
		}
	}

	var out strings.Builder
	err := runReset(storePath, true, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("runReset: %v", err)
	}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, statErr := os.Stat(storePath + suffix); !errors.Is(statErr, fs.ErrNotExist) {
			t.Errorf("expected %s to be deleted, stat err: %v", storePath+suffix, statErr)
		}
	}
}
