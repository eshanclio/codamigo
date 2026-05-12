package openaicompat

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
)

// EmbeddingRequest is the JSON body sent to the embedding API.
type EmbeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
	InputType  string   `json:"input_type,omitempty"`
}

// EmbeddingResponse is the JSON body returned by the embedding API.
type EmbeddingResponse struct {
	Data  []EmbeddingData `json:"data"`
	Usage EmbeddingUsage  `json:"usage"`
}

// EmbeddingData is one embedding vector in the API response.
type EmbeddingData struct {
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

// EmbeddingUsage reports token consumption.
type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens,omitempty"`
	TotalTokens  int `json:"total_tokens"`
}

// APIError is the JSON body returned on non-2xx responses.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("embedding API: HTTP %d: %s", e.StatusCode, e.Body)
}

// CallAPI sends a single embedding request and returns vectors sorted by index.
// The API does not guarantee response ordering, so results are sorted by the
// index field to match the original input order.
func CallAPI(ctx context.Context, client *http.Client, baseURL, apiKey string, req EmbeddingRequest) ([][]float32, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling embedding request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating embedding request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending embedding request: %w", err)
	}
	defer resp.Body.Close()

	const maxResponseSize = 64 * 1024 * 1024 // 64 MB
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return nil, fmt.Errorf("reading embedding response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(respBody)}
	}

	var embResp EmbeddingResponse
	if err := json.Unmarshal(respBody, &embResp); err != nil {
		return nil, fmt.Errorf("decoding embedding response: %w", err)
	}

	if len(embResp.Data) != len(req.Input) {
		return nil, fmt.Errorf("embedding response: got %d vectors, want %d", len(embResp.Data), len(req.Input))
	}

	slices.SortFunc(embResp.Data, func(a, b EmbeddingData) int {
		return cmp.Compare(a.Index, b.Index)
	})

	vectors := make([][]float32, len(embResp.Data))
	for i, d := range embResp.Data {
		vectors[i] = d.Embedding
	}
	return vectors, nil
}
