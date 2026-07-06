package main

import (
	"strings"
	"testing"

	"github.com/ieshan/go-code-chunker/langs"
)

func TestBuildExtensionFilter(t *testing.T) {
	allLangs := langs.AllLanguages()
	filter := buildExtensionFilter(allLangs)

	t.Run("known_exts", func(t *testing.T) {
		// Every extension declared in AllLanguages() must be accepted.
		for _, lang := range allLangs {
			for _, ext := range lang.Extensions {
				if !filter("file" + ext) {
					t.Errorf("filter returned false for declared extension %s (lang %s)", ext, lang.Name)
				}
			}
		}
	})

	t.Run("unknown_exts", func(t *testing.T) {
		unknowns := []string{".ttf", ".csv", ".pdf", ".png", ".lzma", ".brotli", ".exe", ".zip"}
		for _, ext := range unknowns {
			if filter("file" + ext) {
				t.Errorf("filter returned true for unsupported extension %s", ext)
			}
		}
	})

	t.Run("empty_ext", func(t *testing.T) {
		// filepath.Ext("Makefile") == "" and filepath.Ext(".gitignore") == ""
		// so dotfiles are covered by the same guard as extensionless files.
		if filter("Makefile") {
			t.Error("filter returned true for Makefile (no extension)")
		}
		if filter("") {
			t.Error("filter returned true for empty path")
		}
		if filter(".gitignore") {
			t.Error("filter returned true for .gitignore (dotfile, no real extension)")
		}
	})

	t.Run("case_insensitive", func(t *testing.T) {
		// Verify the filter normalises extension case by uppercasing the first
		// extension of every language declared in AllLanguages().
		for _, lang := range allLangs {
			if len(lang.Extensions) == 0 {
				continue
			}
			upper := strings.ToUpper(lang.Extensions[0])
			if !filter("file" + upper) {
				t.Errorf("filter returned false for uppercased extension %s (lang %s, case normalisation broken)", upper, lang.Name)
			}
		}
	})
}
