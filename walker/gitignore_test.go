package walker

import "testing"

func TestGitIgnore_Match(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		isDir    bool
		want     matchResult
	}{
		// Simple globs
		{name: "star glob matches extension", patterns: []string{"*.log"}, path: "app.log", isDir: false, want: matchIgnored},
		{name: "star glob no match", patterns: []string{"*.log"}, path: "app.txt", isDir: false, want: matchNone},
		{name: "question mark glob", patterns: []string{"?.txt"}, path: "a.txt", isDir: false, want: matchIgnored},
		{name: "question mark no match multi char", patterns: []string{"?.txt"}, path: "ab.txt", isDir: false, want: matchNone},
		{name: "character class", patterns: []string{"[abc].txt"}, path: "a.txt", isDir: false, want: matchIgnored},
		{name: "character class no match", patterns: []string{"[abc].txt"}, path: "d.txt", isDir: false, want: matchNone},

		// Directory-only trailing slash
		{name: "dir only matches dir", patterns: []string{"build/"}, path: "build", isDir: true, want: matchIgnored},
		{name: "dir only skips file", patterns: []string{"build/"}, path: "build", isDir: false, want: matchNone},

		// Globstar
		{name: "leading globstar", patterns: []string{"**/test"}, path: "src/pkg/test", isDir: false, want: matchIgnored},
		{name: "trailing globstar", patterns: []string{"src/**"}, path: "src/deep/file.go", isDir: false, want: matchIgnored},
		{name: "middle globstar", patterns: []string{"src/**/test.go"}, path: "src/pkg/sub/test.go", isDir: false, want: matchIgnored},
		{name: "globstar no match", patterns: []string{"src/**/test.go"}, path: "lib/test.go", isDir: false, want: matchNone},

		// Comments and blanks
		{name: "comment line ignored", patterns: []string{"# this is a comment", "*.log"}, path: "app.log", isDir: false, want: matchIgnored},
		{name: "blank line ignored", patterns: []string{"", "*.log"}, path: "app.log", isDir: false, want: matchIgnored},

		// Path anchoring
		{name: "anchored pattern matches at root", patterns: []string{"/build"}, path: "build", isDir: false, want: matchIgnored},
		{name: "anchored pattern no match in subdir", patterns: []string{"/build"}, path: "src/build", isDir: false, want: matchNone},
		{name: "unanchored matches anywhere", patterns: []string{"build"}, path: "src/build", isDir: false, want: matchIgnored},
		{name: "middle slash anchors", patterns: []string{"doc/frotz"}, path: "doc/frotz", isDir: false, want: matchIgnored},
		{name: "middle slash no match elsewhere", patterns: []string{"doc/frotz"}, path: "src/doc/frotz", isDir: false, want: matchNone},

		// Negation
		{name: "negation re-includes", patterns: []string{"*.log", "!important.log"}, path: "important.log", isDir: false, want: matchIncluded},
		{name: "negation does not affect others", patterns: []string{"*.log", "!important.log"}, path: "debug.log", isDir: false, want: matchIgnored},
		{name: "last match wins", patterns: []string{"*.log", "!important.log", "*.log"}, path: "important.log", isDir: false, want: matchIgnored},

		// Escaping
		{name: "escaped hash is literal", patterns: []string{`\#file`}, path: "#file", isDir: false, want: matchIgnored},
		{name: "escaped bang is literal", patterns: []string{`\!file`}, path: "!file", isDir: false, want: matchIgnored},

		// Nested path with unanchored glob
		{name: "unanchored star in nested", patterns: []string{"*.go"}, path: "src/main.go", isDir: false, want: matchIgnored},

		// No patterns means no match
		{name: "empty patterns", patterns: []string{}, path: "anything", isDir: false, want: matchNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gi := compileGitIgnore(tt.patterns)
			got := gi.MatchResult(tt.path, tt.isDir)
			if got != tt.want {
				t.Errorf("MatchResult(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
			}
		})
	}
}

func TestGitIgnore_NegatedCharacterClass(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		path    string
		isDir   bool
		want    matchResult
	}{
		{"negated class excludes matching", "[!abc].txt", "d.txt", false, matchIgnored},
		{"negated class does not exclude non-matching", "[!abc].txt", "a.txt", false, matchNone},
		{"negated range", "*.[!o]", "file.c", false, matchIgnored},
		{"negated range excludes matching", "*.[!o]", "file.o", false, matchNone},
		{"negated alpha range matches non-alpha", "[!a-z].txt", "1.txt", false, matchIgnored},
		{"negated alpha range rejects alpha", "[!a-z].txt", "m.txt", false, matchNone},
		{"positive class still works", "[abc].txt", "a.txt", false, matchIgnored},
		{"positive class rejects non-matching", "[abc].txt", "d.txt", false, matchNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gi := compileGitIgnore([]string{tt.pattern})
			got := gi.MatchResult(tt.path, tt.isDir)
			if got != tt.want {
				t.Errorf("MatchResult(%q, isDir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
			}
		})
	}
}

func TestGitIgnore_MalformedPatternSkipped(t *testing.T) {
	tests := []struct {
		name             string
		pattern          string
		wantPatternCount int // 0 means "don't check"
	}{
		// reversed range: gitignoreToRegex passes it through to regexp.Compile which rejects it.
		{"reversed range", "[z-a].txt", 1},
		// unclosed bracket: gitignoreToRegex escapes the '[', so the pattern compiles
		// to a valid regex matching the literal string "[abc". It is not rejected — 2 patterns survive.
		{"unclosed bracket", "[abc", 2},
		// invalid nested class: same escaping behaviour as unclosed bracket.
		{"invalid nested class", "[[.txt", 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gi := compileGitIgnore([]string{tt.pattern, "*.log"})
			// For patterns that are truly rejected by regexp.Compile, verify the count.
			if tt.wantPatternCount != 0 && len(gi.patterns) != tt.wantPatternCount {
				t.Errorf("expected %d pattern(s) for %q, got %d", tt.wantPatternCount, tt.pattern, len(gi.patterns))
			}
			// The valid pattern (*.log) should still work regardless.
			got := gi.MatchResult("app.log", false)
			if got != matchIgnored {
				t.Errorf("valid pattern after malformed %q: got %v, want matchIgnored", tt.pattern, got)
			}
		})
	}
}

func BenchmarkGitIgnore_Match(b *testing.B) {
	patterns := []string{
		"*.o", "*.a", "*.so", "*.pyc", "__pycache__/",
		"node_modules/", ".git/", "*.log", "build/",
		"dist/", "*.tmp", "*.swp", ".DS_Store",
		"**/vendor", "!vendor/important.go",
	}
	gi := compileGitIgnore(patterns)

	for b.Loop() {
		gi.MatchResult("src/pkg/deep/file.go", false)
	}
}
