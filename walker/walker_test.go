package walker_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ieshan/codamigo/config"
	"github.com/ieshan/codamigo/walker"
)

func setupTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	dirs := []string{"src", "src/utils", "vendor", ".git"}
	for _, d := range dirs {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}

	files := map[string]string{
		"src/main.go":         "package main",
		"src/utils/helper.go": "package utils",
		"vendor/dep.go":       "package dep",
		".git/config":         "[core]",
		"README.md":           "# readme",
		".gitignore":          "vendor/\n",
	}
	for name, content := range files {
		os.WriteFile(filepath.Join(root, name), []byte(content), 0o644)
	}

	return root
}

func TestWalk_BasicTraversal(t *testing.T) {
	root := setupTree(t)
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var paths []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		paths = append(paths, rel)
	}

	// .git/ should always be skipped. vendor/ should be skipped via .gitignore.
	for _, p := range paths {
		if p == ".git" || strings.HasPrefix(p, ".git/") {
			t.Errorf("should skip .git directory, got %s", p)
		}
		if len(p) >= 6 && p[:6] == "vendor" {
			t.Errorf("should skip vendor (gitignored), got %s", p)
		}
	}

	// Should include src/ files and README.
	found := make(map[string]bool)
	for _, p := range paths {
		found[p] = true
	}
	for _, want := range []string{"src/main.go", "src/utils/helper.go", "README.md"} {
		if !found[want] {
			t.Errorf("expected %s in walk results", want)
		}
	}
}

func TestWalk_IncludePatterns(t *testing.T) {
	root := setupTree(t)
	cfg := &config.Config{
		ProjectRoot:     root,
		IncludePatterns: []string{"*.go"},
	}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		if filepath.Ext(path) != ".go" {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("include pattern *.go should exclude %s", rel)
		}
	}
}

func TestWalk_ExcludePatterns(t *testing.T) {
	root := setupTree(t)
	cfg := &config.Config{
		ProjectRoot:     root,
		ExcludePatterns: []string{"*.md"},
	}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		if filepath.Ext(path) == ".md" {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("exclude pattern *.md should exclude %s", rel)
		}
	}
}

func TestWalk_ContextCancellation(t *testing.T) {
	root := setupTree(t)
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	count := 0
	for _, err = range w.Walk(ctx) {
		if err != nil {
			break
		}
		count++
	}
	// With an already-cancelled context, we should get 0 or very few paths.
	if count > 1 {
		t.Errorf("expected 0-1 paths with cancelled context, got %d", count)
	}
}

func TestMatch(t *testing.T) {
	root := setupTree(t)
	cfg := &config.Config{
		ProjectRoot:     root,
		ExcludePatterns: []string{"*.md"},
	}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	if !w.Match(filepath.Join(root, "src/main.go")) {
		t.Error("Match should return true for src/main.go")
	}
	if w.Match(filepath.Join(root, "README.md")) {
		t.Error("Match should return false for excluded README.md")
	}
	if w.Match(filepath.Join(root, "vendor/dep.go")) {
		t.Error("Match should return false for gitignored vendor/dep.go")
	}
	if w.Match(filepath.Join(root, ".git/config")) {
		t.Error("Match should return false for .git/config")
	}
}

func TestWalk_SkipsCodamigoDir(t *testing.T) {
	root := t.TempDir()

	// Files that SHOULD be walked.
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Files inside .codamigo/ that must NOT be walked.
	codamigoDir := filepath.Join(root, ".codamigo")
	if err := os.MkdirAll(codamigoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codamigoDir, "store.db"), []byte("sqlite data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codamigoDir, "settings.yml"), []byte("store_path: x"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := walker.New(root, &config.Config{ProjectRoot: root})
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		if strings.HasPrefix(rel, ".codamigo") {
			t.Errorf("walker yielded .codamigo path: %s", rel)
		}
	}
}

func TestMatch_SkipsCodamigoDir(t *testing.T) {
	root := t.TempDir()
	w, err := walker.New(root, &config.Config{ProjectRoot: root})
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	codamigoPath := filepath.Join(root, ".codamigo", "store.db")
	if w.Match(codamigoPath) {
		t.Error("Match returned true for .codamigo/store.db, want false")
	}
}

func TestWalk_NestedGitignore(t *testing.T) {
	root := t.TempDir()

	// Create directory structure.
	dirs := []string{"src", "src/vendor", "src/lib", "build"}
	for _, d := range dirs {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}

	// Root .gitignore: ignore build/
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("build/\n"), 0o644)

	// Nested .gitignore in src/: ignore vendor/
	os.WriteFile(filepath.Join(root, "src", ".gitignore"), []byte("vendor/\n"), 0o644)

	// Create files.
	files := []string{
		"main.go",
		"src/app.go",
		"src/vendor/dep.go",
		"src/lib/util.go",
		"build/output.go",
	}
	for _, f := range files {
		os.WriteFile(filepath.Join(root, f), []byte("package x"), 0o644)
	}

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var walked []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		walked = append(walked, filepath.ToSlash(rel))
	}

	slices.Sort(walked)

	// Should include main.go, src/app.go, src/lib/util.go
	// Should exclude src/vendor/dep.go (nested .gitignore) and build/output.go (root .gitignore)
	want := []string{"main.go", "src/app.go", "src/lib/util.go"}
	if !slices.Equal(walked, want) {
		t.Errorf("walked = %v, want %v", walked, want)
	}
}

func TestWalker_IsIgnored(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "src", "vendor"), 0o755)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n"), 0o644)
	os.WriteFile(filepath.Join(root, "src", ".gitignore"), []byte("vendor/\n"), 0o644)

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	tests := []struct {
		path string
		want bool
	}{
		{"src/app.go", false},
		{"src/vendor/dep.go", true},
		{"debug.log", true},
		{"src/main.go", false},
	}
	for _, tt := range tests {
		abs := filepath.Join(root, tt.path)
		got := w.IsIgnored(abs)
		if got != tt.want {
			t.Errorf("IsIgnored(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestWalk_NestedGitignoreNegation(t *testing.T) {
	root := t.TempDir()

	dirs := []string{"src", "src/generated", "src/generated/keep"}
	for _, d := range dirs {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}

	// Root .gitignore: ignore all generated/ dirs.
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("generated/\n"), 0o644)

	// Nested .gitignore in src/: re-include generated/ via negation.
	os.WriteFile(filepath.Join(root, "src", ".gitignore"), []byte("!generated/\n"), 0o644)

	files := []string{
		"main.go",
		"src/app.go",
		"src/generated/models.go",
	}
	for _, f := range files {
		os.WriteFile(filepath.Join(root, f), []byte("package x"), 0o644)
	}

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var walked []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		walked = append(walked, filepath.ToSlash(rel))
	}

	slices.Sort(walked)

	// The deeper negation rule in src/.gitignore should override the root ignore.
	want := []string{"main.go", "src/app.go", "src/generated/models.go"}
	if !slices.Equal(walked, want) {
		t.Errorf("walked = %v, want %v", walked, want)
	}
}

func TestWalk_CaignoreOnly(t *testing.T) {
	root := t.TempDir()

	os.MkdirAll(filepath.Join(root, "vendor"), 0o755)
	os.WriteFile(filepath.Join(root, "vendor", "dep.go"), []byte("package dep"), 0o644)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644)
	// No .gitignore — only .caignore.
	os.WriteFile(filepath.Join(root, ".caignore"), []byte("vendor/\n"), 0o644)

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var walked []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		walked = append(walked, filepath.ToSlash(rel))
	}

	slices.Sort(walked)

	want := []string{"main.go"}
	if !slices.Equal(walked, want) {
		t.Errorf("walked = %v, want %v", walked, want)
	}
}

func TestWalk_CaignoreExtends(t *testing.T) {
	root := t.TempDir()

	os.MkdirAll(filepath.Join(root, "logs"), 0o755)
	os.MkdirAll(filepath.Join(root, "tmp"), 0o755)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(root, "logs", "app.log"), []byte("log data"), 0o644)
	os.WriteFile(filepath.Join(root, "tmp", "scratch.txt"), []byte("temp"), 0o644)
	// .gitignore ignores logs/
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("logs/\n"), 0o644)
	// .caignore additionally ignores tmp/
	os.WriteFile(filepath.Join(root, ".caignore"), []byte("tmp/\n"), 0o644)

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var walked []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		walked = append(walked, filepath.ToSlash(rel))
	}

	slices.Sort(walked)

	// Both logs/ and tmp/ should be excluded.
	want := []string{"main.go"}
	if !slices.Equal(walked, want) {
		t.Errorf("walked = %v, want %v", walked, want)
	}
}

func TestWalk_CaignoreNegationOverride(t *testing.T) {
	root := t.TempDir()

	os.MkdirAll(filepath.Join(root, "generated"), 0o755)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(root, "generated", "models.go"), []byte("package gen"), 0o644)
	// .gitignore ignores generated/
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("generated/\n"), 0o644)
	// .caignore re-includes generated/ via negation.
	os.WriteFile(filepath.Join(root, ".caignore"), []byte("!generated/\n"), 0o644)

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var walked []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		walked = append(walked, filepath.ToSlash(rel))
	}

	slices.Sort(walked)

	// .caignore negation should re-include generated/.
	want := []string{"generated/models.go", "main.go"}
	if !slices.Equal(walked, want) {
		t.Errorf("walked = %v, want %v", walked, want)
	}
}

func TestWalk_CaignoreFileNotYielded(t *testing.T) {
	root := t.TempDir()

	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(root, ".caignore"), []byte("*.tmp\n"), 0o644)

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		if rel == ".caignore" {
			t.Error(".caignore file should not be yielded by Walk")
		}
	}
}

func TestWalker_CaignoreMatchAndIsIgnored(t *testing.T) {
	root := t.TempDir()

	os.MkdirAll(filepath.Join(root, "cache"), 0o755)
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(root, "cache", "data.bin"), []byte("binary"), 0o644)
	// No .gitignore. .caignore ignores cache/.
	os.WriteFile(filepath.Join(root, ".caignore"), []byte("cache/\n"), 0o644)

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	if !w.Match(filepath.Join(root, "main.go")) {
		t.Error("Match should return true for main.go")
	}
	if w.Match(filepath.Join(root, "cache", "data.bin")) {
		t.Error("Match should return false for cache/data.bin (caignored)")
	}
	if !w.IsIgnored(filepath.Join(root, "cache", "data.bin")) {
		t.Error("IsIgnored should return true for cache/data.bin (caignored)")
	}
	if w.IsIgnored(filepath.Join(root, "main.go")) {
		t.Error("IsIgnored should return false for main.go")
	}
}

func TestWalk_NestedCaignore(t *testing.T) {
	root := t.TempDir()

	dirs := []string{"src", "src/fixtures", "lib"}
	for _, d := range dirs {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}

	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(root, "src", "app.go"), []byte("package src"), 0o644)
	os.WriteFile(filepath.Join(root, "src", "fixtures", "data.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, "lib", "util.go"), []byte("package lib"), 0o644)
	// Nested .caignore in src/ ignores fixtures/ (only applies under src/).
	os.WriteFile(filepath.Join(root, "src", ".caignore"), []byte("fixtures/\n"), 0o644)

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var walked []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		walked = append(walked, filepath.ToSlash(rel))
	}

	slices.Sort(walked)

	// src/fixtures/data.json should be excluded by nested .caignore.
	want := []string{"lib/util.go", "main.go", "src/app.go"}
	if !slices.Equal(walked, want) {
		t.Errorf("walked = %v, want %v", walked, want)
	}
}

func TestWalker_GitignorePartialReadOnScannerError(t *testing.T) {
	root := t.TempDir()

	// Write a .gitignore with a valid first line followed by a line exceeding
	// bufio.Scanner's default 64 KB buffer. readIgnoreFile should return the
	// partial lines collected before the error, so *.log must still be applied.
	gitignoreContent := "*.log\n" + strings.Repeat("x", 65*1024) + "\n"
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(gitignoreContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// app.log should be ignored via the first (valid) line.
	if err := os.WriteFile(filepath.Join(root, "app.log"), []byte("log data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// main.go should NOT be ignored.
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var walked []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		walked = append(walked, rel)
	}

	for _, p := range walked {
		if strings.HasSuffix(p, ".log") {
			t.Errorf("app.log should be ignored by partial .gitignore rules, but got %s", p)
		}
	}

	found := false
	for _, p := range walked {
		if p == "main.go" {
			found = true
			break
		}
	}
	if !found {
		t.Error("main.go should be walked but was not found")
	}
}

func TestWalker_FSPanicsAfterClose(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}

	if err = w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				msg, ok := r.(string)
				if !ok || msg != "walker: FS called after Close" {
					t.Errorf("FS() panicked with unexpected value: %v", r)
				}
				panicked = true
			}
		}()
		w.FS()
	}()

	if !panicked {
		t.Error("expected FS() to panic after Close, but it did not")
	}
}

func TestWalk_Symlinks(t *testing.T) {
	root := t.TempDir()

	// Real directory and file inside root.
	if err := os.MkdirAll(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "subdir", "inner.go"), []byte("package sub"), 0o644); err != nil {
		t.Fatalf("write inner.go: %v", err)
	}

	// Symlink -> directory (relative target, within root).
	if err := os.Symlink("subdir", filepath.Join(root, "link-to-dir")); err != nil {
		t.Fatalf("symlink link-to-dir: %v", err)
	}
	// Symlink -> regular file (relative target, within root).
	if err := os.Symlink("main.go", filepath.Join(root, "link-to-file.go")); err != nil {
		t.Fatalf("symlink link-to-file.go: %v", err)
	}
	// Dangling symlink (target does not exist).
	if err := os.Symlink("nonexistent", filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("symlink dangling: %v", err)
	}

	cfg := &config.Config{ProjectRoot: root}
	w, err := walker.New(root, cfg)
	if err != nil {
		t.Fatalf("walker.New: %v", err)
	}
	defer w.Close()

	var yielded []string
	for path, err := range w.Walk(t.Context()) {
		if err != nil {
			t.Fatalf("walk error: %v", err)
		}
		rel, _ := filepath.Rel(root, path)
		yielded = append(yielded, filepath.ToSlash(rel))
	}

	// Symlink-to-directory must NOT be yielded.
	if slices.Contains(yielded, "link-to-dir") {
		t.Error("link-to-dir (symlink to directory) must not be yielded")
	}
	// Dangling symlink must NOT be yielded.
	if slices.Contains(yielded, "dangling") {
		t.Error("dangling symlink must not be yielded")
	}
	// Symlink-to-file MUST be yielded (regression guard).
	if !slices.Contains(yielded, "link-to-file.go") {
		t.Errorf("link-to-file.go (symlink to file) must be yielded; got %v", yielded)
	}
	// Regular file MUST be yielded.
	if !slices.Contains(yielded, "main.go") {
		t.Errorf("main.go must be yielded; got %v", yielded)
	}
	// Real directory contents must still be yielded via the real path.
	if !slices.Contains(yielded, "subdir/inner.go") {
		t.Errorf("subdir/inner.go must be yielded via real directory; got %v", yielded)
	}
}

func TestConcurrentWalkAndMatch(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a"), 0o644)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "b.go"), []byte("package b"), 0o644)

	cfg := config.Defaults()
	w, err := walker.New(dir, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	// Walk in one goroutine.
	wg.Go(func() {
		for range w.Walk(ctx) {
		}
	})

	// Match concurrently in another goroutine.
	wg.Go(func() {
		for range 100 {
			w.Match(filepath.Join(dir, "a.go"))
			w.Match(filepath.Join(dir, "sub", "b.go"))
		}
	})

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success — no deadlock
	case <-ctx.Done():
		t.Fatal("deadlock detected: concurrent Walk+Match did not complete")
	}
}

func TestWithFileFilter(t *testing.T) {
	// filter accepts only .go, .py, .rb files (lowercase-normalised).
	filter := func(path string) bool {
		ext := strings.ToLower(filepath.Ext(path))
		return ext == ".go" || ext == ".py" || ext == ".rb"
	}

	t.Run("accepted", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
			t.Fatal(err)
		}
		w, err := walker.New(root, &config.Config{ProjectRoot: root}, walker.WithFileFilter(filter))
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		var yielded []string
		for path, err := range w.Walk(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, path)
			yielded = append(yielded, rel)
		}
		if !slices.Contains(yielded, "main.go") {
			t.Errorf("main.go should be yielded by filter, got %v", yielded)
		}
	})

	t.Run("rejected", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "font.ttf"), []byte("binary"), 0o644); err != nil {
			t.Fatal(err)
		}
		w, err := walker.New(root, &config.Config{ProjectRoot: root}, walker.WithFileFilter(filter))
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		for path, err := range w.Walk(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, path)
			t.Errorf("expected no files yielded, got %s", rel)
		}
	})

	t.Run("empty_ext", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte("all:"), 0o644); err != nil {
			t.Fatal(err)
		}
		w, err := walker.New(root, &config.Config{ProjectRoot: root}, walker.WithFileFilter(filter))
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		for path, err := range w.Walk(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, path)
			t.Errorf("Makefile (no extension) should be rejected, got %s", rel)
		}
	})

	t.Run("nil_filter", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "font.ttf"), []byte("binary"), 0o644); err != nil {
			t.Fatal(err)
		}
		// No WithFileFilter option — all files must be yielded (backward-compat).
		w, err := walker.New(root, &config.Config{ProjectRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		var yielded []string
		for path, err := range w.Walk(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, path)
			yielded = append(yielded, rel)
		}
		if !slices.Contains(yielded, "font.ttf") {
			t.Errorf("font.ttf must be yielded with no filter, got %v", yielded)
		}
	})

	t.Run("match_method", func(t *testing.T) {
		root := t.TempDir()
		w, err := walker.New(root, &config.Config{ProjectRoot: root}, walker.WithFileFilter(filter))
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		if w.Match(filepath.Join(root, "font.ttf")) {
			t.Error("Match should return false for .ttf with extension filter")
		}
		if !w.Match(filepath.Join(root, "main.go")) {
			t.Error("Match should return true for .go with extension filter")
		}
	})

	t.Run("index_files_path", func(t *testing.T) {
		// Simulates the IndexFiles incremental path: Match is called on a path
		// with an unsupported extension supplied directly by the caller.
		root := t.TempDir()
		w, err := walker.New(root, &config.Config{ProjectRoot: root}, walker.WithFileFilter(filter))
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		if w.Match(filepath.Join(root, "data.csv")) {
			t.Error("Match should return false for .csv (unsupported extension via IndexFiles path)")
		}
	})

	t.Run("case_insensitive", func(t *testing.T) {
		root := t.TempDir()
		files := []string{"main.Go", "script.PY", "app.RB"}
		for _, f := range files {
			if err := os.WriteFile(filepath.Join(root, f), []byte("content"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		w, err := walker.New(root, &config.Config{ProjectRoot: root}, walker.WithFileFilter(filter))
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		var yielded []string
		for path, err := range w.Walk(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, path)
			yielded = append(yielded, rel)
		}
		for _, f := range files {
			if !slices.Contains(yielded, f) {
				t.Errorf("expected %s yielded (uppercase ext normalised by filter), got %v", f, yielded)
			}
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		root := t.TempDir()
		for i := range 10 {
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file%d.go", i)), []byte("package main"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("font%d.ttf", i)), []byte("binary"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		w, err := walker.New(root, &config.Config{ProjectRoot: root}, walker.WithFileFilter(filter))
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()

		var wg sync.WaitGroup

		// One goroutine walks; filter must reject all .ttf files.
		wg.Go(func() {
			for path, walkErr := range w.Walk(ctx) {
				if walkErr != nil {
					return
				}
				if filepath.Ext(path) == ".ttf" {
					t.Errorf("ttf file yielded by Walk with filter active: %s", path)
				}
			}
		})

		// 19 goroutines call Match concurrently with the Walk above.
		for range 19 {
			wg.Go(func() {
				for range 50 {
					if !w.Match(filepath.Join(root, "file0.go")) {
						t.Errorf("Match returned false for supported .go file")
					}
					if w.Match(filepath.Join(root, "font0.ttf")) {
						t.Errorf("Match returned true for unsupported .ttf file")
					}
				}
			})
		}

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("deadlock or timeout detected in concurrent Walk+Match with extension filter")
		}
	})
}
