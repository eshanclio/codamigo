package chunker_test

import (
	"fmt"

	"github.com/ieshan/codamigo/chunker"
	"github.com/ieshan/codamigo/langs"
)

func ExampleNewChunker() {
	c, err := chunker.NewChunker(langs.AllLanguages(), chunker.DefaultConfig())
	if err != nil {
		panic(err)
	}
	_ = c
	fmt.Println("chunker ready")
	// Output: chunker ready
}

func ExampleChunker_ChunkFile() {
	c, err := chunker.NewChunker(
		[]chunker.LanguageConfig{langs.GoLanguage()},
		chunker.DefaultConfig(),
	)
	if err != nil {
		panic(err)
	}

	src := `package main

func Hello() string {
	return "hello"
}
`
	chunks, err := c.ChunkFile("main.go", []byte(src))
	if err != nil {
		panic(err)
	}
	for _, ch := range chunks {
		fmt.Printf("kind=%s name=%s\n", ch.NodeKind, ch.Name)
	}
	// Output: kind=function_declaration name=Hello
}
