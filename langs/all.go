// Package langs provides [chunker.LanguageConfig] definitions for the 15
// languages built into codamigo. Each language is implemented in its own
// file (go.go, python.go, …) and lazily initialised via [sync.OnceValue]
// so the CGo grammars are loaded only once per process.
//
// Import this package only from cmd/codamigo. All other packages receive
// language support through a [*chunker.Chunker] injected at construction
// time, keeping CGo out of the library layer.
package langs

import "github.com/ieshan/codamigo/chunker"

// AllLanguages returns LanguageConfigs for all 15 built-in languages.
// Pass this to chunker.NewChunker for full language coverage.
// To add a language not listed here, append your LanguageConfig to the returned slice.
func AllLanguages() []chunker.LanguageConfig {
	return []chunker.LanguageConfig{
		GoLanguage(),
		PythonLanguage(),
		JavaScriptLanguage(),
		TypeScriptLanguage(),
		TSXLanguage(),
		RubyLanguage(),
		CLanguage(),
		CppLanguage(),
		BashLanguage(),
		HTMLLanguage(),
		CSSLanguage(),
		MarkdownLanguage(),
		JSONLanguage(),
		YAMLLanguage(),
		VueLanguage(),
	}
}
