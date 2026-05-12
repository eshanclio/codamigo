// Package embedder defines the Embedder interface for converting text into
// float32 embedding vectors.
//
// This package contains only the interface. Concrete implementations live in
// sub-packages (e.g. embedder/openaicompat) and are constructed in
// cmd/codamigo. Keeping implementations in sub-packages prevents provider-
// specific HTTP logic from leaking into library consumers.
package embedder

import "context"

// Embedder converts text into float32 embedding vectors.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}
