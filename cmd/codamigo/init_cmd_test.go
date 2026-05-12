package main

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadPrompt_AcceptsInput(t *testing.T) {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader("myvalue\n"))
	got := readPrompt(scanner, &out, "Base URL", "https://default.example")
	if got != "myvalue" {
		t.Errorf("want %q, got %q", "myvalue", got)
	}
}

func TestReadPrompt_UsesDefault(t *testing.T) {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader("\n"))
	got := readPrompt(scanner, &out, "Base URL", "https://default.example")
	if got != "https://default.example" {
		t.Errorf("want %q, got %q", "https://default.example", got)
	}
	if !strings.Contains(out.String(), "https://default.example") {
		t.Errorf("prompt should display the default; got: %q", out.String())
	}
}

func TestReadPrompt_TrimsWhitespace(t *testing.T) {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader("  hello  \n"))
	got := readPrompt(scanner, &out, "Key", "")
	if got != "hello" {
		t.Errorf("want %q, got %q", "hello", got)
	}
}

func TestReadPrompt_MultipleCallsSharedScanner(t *testing.T) {
	var out strings.Builder
	scanner := bufio.NewScanner(strings.NewReader("url\nmodel\nkey\n"))
	v1 := readPrompt(scanner, &out, "Base URL", "default1")
	v2 := readPrompt(scanner, &out, "Model", "default2")
	v3 := readPrompt(scanner, &out, "Key", "default3")
	if v1 != "url" {
		t.Errorf("v1: want %q, got %q", "url", v1)
	}
	if v2 != "model" {
		t.Errorf("v2: want %q, got %q", "model", v2)
	}
	if v3 != "key" {
		t.Errorf("v3: want %q, got %q", "key", v3)
	}
}

func TestAppendToGitignore_CreatesEntry(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := appendToGitignore(dir); err != nil {
		t.Fatalf("appendToGitignore: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if !strings.Contains(string(content), ".codamigo/") {
		t.Errorf(".gitignore should contain .codamigo/, got: %q", string(content))
	}
}

func TestAppendToGitignore_IdempotentIfAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitignore := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gitignore, []byte(".codamigo/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendToGitignore(dir); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	count := strings.Count(string(content), ".codamigo/")
	if count != 1 {
		t.Errorf("expected entry once, found %d times", count)
	}
}

func TestAppendToGitignore_NoOpWithoutGitDir(t *testing.T) {
	dir := t.TempDir() // no .git subdirectory
	if err := appendToGitignore(dir); err != nil {
		t.Fatalf("should be no-op, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !errors.Is(err, fs.ErrNotExist) {
		t.Error(".gitignore should not have been created without a .git dir")
	}
}

func TestSafeConfigPath(t *testing.T) {
	base := t.TempDir()

	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"valid relative", filepath.Join(base, "config.yml"), false},
		{"valid subdirectory", filepath.Join(base, "sub", "config.yml"), false},
		{"escapes via ..", filepath.Join(base, "..", "outside.yml"), true},
		{"absolute outside base", "/etc/passwd", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := safeConfigPath(base, tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("safeConfigPath(%q, %q) error = %v, wantErr %v", base, tt.path, err, tt.wantErr)
			}
		})
	}
}
