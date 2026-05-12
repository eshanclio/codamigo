package walker

import (
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
)

// compiledPattern is a pre-compiled gitignore rule ready for fast regex matching.
type compiledPattern struct {
	regex   *regexp.Regexp
	negate  bool // true when the pattern starts with "!" (re-includes previously ignored paths)
	dirOnly bool // true when the pattern ends with "/" (matches directories only)
}

// gitIgnore holds the compiled patterns parsed from a single .gitignore file.
type gitIgnore struct {
	patterns []compiledPattern
}

// compileGitIgnore parses raw .gitignore lines into a gitIgnore ready for matching.
// Blank lines and comment lines (starting with #) are skipped. Escaped leading
// # and ! are unescaped. Pattern compilation errors are logged and skipped.
func compileGitIgnore(lines []string) *gitIgnore {
	patterns := make([]compiledPattern, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if line == "" {
			continue
		}
		// Handle escaping of leading # and !
		if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
			line = line[1:]
		} else if strings.HasPrefix(line, "#") {
			continue
		} else if strings.HasPrefix(line, "!") {
			line = line[1:]
			p := compiledPattern{negate: true}
			if strings.HasSuffix(line, "/") {
				p.dirOnly = true
				line = strings.TrimSuffix(line, "/")
			}
			anchored := strings.Contains(line, "/")
			line = strings.TrimPrefix(line, "/")
			re, err := regexp.Compile(gitignoreToRegex(line, anchored))
			if err != nil {
				slog.Warn("skipping invalid gitignore pattern",
					slog.String("pattern", line), slog.Any("error", err))
				continue
			}
			p.regex = re
			patterns = append(patterns, p)
			continue
		}

		p := compiledPattern{}
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		anchored := strings.Contains(line, "/")
		line = strings.TrimPrefix(line, "/")
		re, err := regexp.Compile(gitignoreToRegex(line, anchored))
		if err != nil {
			slog.Warn("skipping invalid gitignore pattern",
				slog.String("pattern", line), slog.Any("error", err))
			continue
		}
		p.regex = re
		patterns = append(patterns, p)
	}
	return &gitIgnore{patterns: patterns}
}

// gitignoreToRegex converts a single gitignore glob pattern to a Go regular
// expression string. anchored is true when the pattern contains a "/" (other
// than a trailing one), meaning it is relative to the .gitignore directory
// rather than matching anywhere in the tree. The resulting regex is anchored
// with ^ and $ and must be matched against slash-normalised relative paths.
func gitignoreToRegex(pattern string, anchored bool) string {
	var b strings.Builder
	b.WriteString("^")

	if !anchored {
		b.WriteString("(.*/)?")
	}

	i := 0
	n := len(pattern)
	for i < n {
		switch {
		case i+3 <= n-1 && pattern[i:i+4] == "/**/":
			b.WriteString("/(.+/)?")
			i += 4
		case i+2 <= n-1 && pattern[i:i+3] == "**/":
			b.WriteString("(.+/)?")
			i += 3
		case i+2 <= n-1 && pattern[i:i+3] == "/**" && i+3 == n:
			b.WriteString("/.*")
			i += 3
		case i+1 <= n-1 && pattern[i] == '*' && pattern[i+1] == '*':
			b.WriteString(".*")
			i += 2
		case pattern[i] == '*':
			b.WriteString("[^/]*")
			i++
		case pattern[i] == '?':
			b.WriteString("[^/]")
			i++
		case pattern[i] == '[':
			end := strings.IndexByte(pattern[i:], ']')
			if end == -1 {
				b.WriteString(regexp.QuoteMeta(string(pattern[i])))
				i++
			} else {
				class := pattern[i : i+end+1]
				if len(class) > 2 && class[1] == '!' {
					class = "[^" + class[2:]
				}
				b.WriteString(class)
				i += end + 1
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}

	b.WriteString("$")
	return b.String()
}

// matchResult is a tri-state return from gitIgnore.MatchResult, enabling
// stacked evaluation where a deeper layer's negation can override a
// shallower layer's ignore (Git semantics: last matching rule wins).
type matchResult int

const (
	matchNone     matchResult = iota // no rule matched
	matchIgnored                     // matched by a non-negated rule
	matchIncluded                    // matched by a negated rule (re-included)
)

// MatchResult evaluates all compiled patterns against path in order, with later
// rules overriding earlier ones (Git semantics). isDir must be true when path
// refers to a directory so that dirOnly patterns are applied correctly.
func (g *gitIgnore) MatchResult(path string, isDir bool) matchResult {
	path = filepath.ToSlash(path)

	result := matchNone
	for _, p := range g.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if p.regex.MatchString(path) {
			if p.negate {
				result = matchIncluded
			} else {
				result = matchIgnored
			}
		}
	}
	return result
}
