package store

import (
	"strings"
	"unicode"
)

// TokenizeForSearch expands camelCase and snake_case identifiers for FTS5 indexing.
// Applied identically on both insert and query paths for consistent matching.
func TokenizeForSearch(text string) string {
	if text == "" {
		return ""
	}

	words := strings.Fields(text)
	var result []string

	for _, word := range words {
		if !containsLetter(word) {
			result = append(result, word)
			continue
		}
		// Strip leading/trailing non-letter, non-digit, non-underscore characters
		// so that "parseConfig()" is treated as identifier "parseConfig" with
		// trailing punctuation "()".
		prefix, ident, suffix := splitPunctuation(word)
		if ident == "" {
			result = append(result, word)
			continue
		}
		if prefix != "" {
			result = append(result, prefix)
		}
		expanded := expandIdentifier(ident)
		result = append(result, expanded...)
		// Suffix punctuation attached to an identifier is dropped — it adds no
		// search value (e.g. the "()" in "parseConfig()").
		_ = suffix
	}

	return strings.Join(result, " ")
}

// expandIdentifier splits word on snake_case boundaries, then further splits
// each part on camelCase boundaries, and returns the unique lowercased tokens
// (including the full word) in a stable order. The full word always appears
// first so prefix searches still match the original identifier.
func expandIdentifier(word string) []string {
	parts := splitSnakeCase(word)

	var allParts []string
	for _, part := range parts {
		camelParts := splitCamelCase(part)
		allParts = append(allParts, camelParts...)
	}

	lower := strings.ToLower(word)
	if len(allParts) <= 1 {
		return []string{lower}
	}

	seen := make(map[string]struct{}, len(allParts)+len(parts)+1)
	var result []string
	add := func(s string) {
		s = strings.ToLower(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		result = append(result, s)
	}

	if strings.Contains(word, "_") {
		add(lower)
		for _, part := range parts {
			camelParts := splitCamelCase(part)
			if len(camelParts) > 1 {
				add(strings.ToLower(part))
				for _, cp := range camelParts {
					add(cp)
				}
			} else {
				add(part)
			}
		}
	} else {
		add(lower)
		for _, p := range allParts {
			add(p)
		}
	}

	return result
}

func splitSnakeCase(s string) []string {
	if !strings.Contains(s, "_") {
		return []string{s}
	}
	parts := strings.Split(s, "_")
	var result []string
	for _, p := range parts {
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func splitCamelCase(s string) []string {
	if s == "" {
		return nil
	}

	runes := []rune(s)
	var parts []string
	start := 0

	for i := 1; i < len(runes); i++ {
		if unicode.IsUpper(runes[i]) && !unicode.IsUpper(runes[i-1]) {
			parts = append(parts, string(runes[start:i]))
			start = i
		} else if i > 1 && unicode.IsUpper(runes[i-1]) && unicode.IsUpper(runes[start]) && unicode.IsLower(runes[i]) {
			parts = append(parts, string(runes[start:i-1]))
			start = i - 1
		}
	}

	parts = append(parts, string(runes[start:]))

	var result []string
	for _, p := range parts {
		if p != "" {
			result = append(result, strings.ToLower(p))
		}
	}
	return result
}

func containsLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// splitPunctuation separates leading/trailing non-identifier characters from s.
// An identifier character is a letter, digit, or underscore.
// Returns (prefix, identifier, suffix).
func splitPunctuation(s string) (prefix, ident, suffix string) {
	runes := []rune(s)
	start := 0
	for start < len(runes) && !isIdentRune(runes[start]) {
		start++
	}
	end := len(runes)
	for end > start && !isIdentRune(runes[end-1]) {
		end--
	}
	return string(runes[:start]), string(runes[start:end]), string(runes[end:])
}

func isIdentRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}
