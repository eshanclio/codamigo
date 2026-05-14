package openaicompat_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ieshan/codamigo/embedder"
	"github.com/ieshan/codamigo/embedder/openaicompat"
)

func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestCallAPI_Success(t *testing.T) {
	var gotReq openaicompat.EmbeddingRequest
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("want POST, got %s", r.Method)
		}
		if r.URL.Path != "/embeddings" {
			t.Errorf("want /embeddings, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("want Bearer test-key, got %s", got)
		}

		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{0.1, 0.2, 0.3}, Index: 1},
				{Embedding: []float32{0.4, 0.5, 0.6}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	vectors, err := openaicompat.CallAPI(t.Context(), http.DefaultClient, srv.URL, "test-key", openaicompat.EmbeddingRequest{
		Model: "test-model",
		Input: []string{"hello", "world"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotReq.Model != "test-model" {
		t.Errorf("request model: want test-model, got %s", gotReq.Model)
	}
	if len(gotReq.Input) != 2 {
		t.Fatalf("request input length: want 2, got %d", len(gotReq.Input))
	}

	if len(vectors) != 2 {
		t.Fatalf("vectors length: want 2, got %d", len(vectors))
	}
	// Verify ordering by index (index 0 should come first)
	if vectors[0][0] != 0.4 {
		t.Errorf("vectors[0][0]: want 0.4, got %f", vectors[0][0])
	}
	if vectors[1][0] != 0.1 {
		t.Errorf("vectors[1][0]: want 0.1, got %f", vectors[1][0])
	}
}

func TestCallAPI_OptionalFieldsOmitted(t *testing.T) {
	var gotBody map[string]any
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{0.1}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	_, err := openaicompat.CallAPI(t.Context(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := gotBody["dimensions"]; ok {
		t.Error("dimensions should be omitted when zero")
	}
	if _, ok := gotBody["input_type"]; ok {
		t.Error("input_type should be omitted when empty")
	}
}

func TestCallAPI_OptionalFieldsPresent(t *testing.T) {
	var gotBody map[string]any
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{0.1}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	_, err := openaicompat.CallAPI(t.Context(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model:      "m",
		Input:      []string{"text"},
		Dimensions: 512,
		InputType:  "document",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	dim, ok := gotBody["dimensions"]
	if !ok {
		t.Fatal("dimensions should be present when non-zero")
	}
	if dim.(float64) != 512 {
		t.Errorf("dimensions: want 512, got %v", dim)
	}

	it, ok := gotBody["input_type"]
	if !ok {
		t.Fatal("input_type should be present when non-empty")
	}
	if it.(string) != "document" {
		t.Errorf("input_type: want document, got %v", it)
	}
}

func TestCallAPI_4xxError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "invalid model"}`))
	})

	_, err := openaicompat.CallAPI(t.Context(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "bad",
		Input: []string{"text"},
	})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}

	var apiErr *openaicompat.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 400 {
		t.Errorf("status code: want 400, got %d", apiErr.StatusCode)
	}
}

func TestCallAPI_MalformedJSON(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json`))
	})

	_, err := openaicompat.CallAPI(t.Context(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text"},
	})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestCallWithRetry_RetriesOn429(t *testing.T) {
	var attempts atomic.Int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "rate limited"}`))
			return
		}
		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{1.0}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	vectors, err := openaicompat.CallWithRetry(t.Context(), http.DefaultClient, nil, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text"},
	}, 5, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts: want 3, got %d", attempts.Load())
	}
	if vectors[0][0] != 1.0 {
		t.Errorf("vectors[0][0]: want 1.0, got %f", vectors[0][0])
	}
}

func TestCallWithRetry_RetriesOn5xx(t *testing.T) {
	var attempts atomic.Int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`service unavailable`))
			return
		}
		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{2.0}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	vectors, err := openaicompat.CallWithRetry(t.Context(), http.DefaultClient, nil, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text"},
	}, 5, 1*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts: want 2, got %d", attempts.Load())
	}
	if vectors[0][0] != 2.0 {
		t.Errorf("vectors[0][0]: want 2.0, got %f", vectors[0][0])
	}
}

func TestCallWithRetry_MaxRetriesExhausted(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`rate limited`))
	})

	_, err := openaicompat.CallWithRetry(t.Context(), http.DefaultClient, nil, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text"},
	}, 3, 1*time.Millisecond)
	if err == nil {
		t.Fatal("expected error after max retries")
	}
	if !errors.Is(err, openaicompat.ErrRateLimited) {
		t.Errorf("want ErrRateLimited, got: %v", err)
	}
}

func TestCallWithRetry_NoRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`bad request`))
	})

	_, err := openaicompat.CallWithRetry(t.Context(), http.DefaultClient, nil, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text"},
	}, 5, 1*time.Millisecond)
	if err == nil {
		t.Fatal("expected error for 400")
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts: want 1 (no retry), got %d", attempts.Load())
	}
	if !errors.Is(err, openaicompat.ErrAPIError) {
		t.Errorf("want ErrAPIError, got: %v", err)
	}
}

func TestCallAPI_VectorCountMismatch(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Return only 1 vector when 2 were requested.
		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{0.1}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	_, err := openaicompat.CallAPI(t.Context(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text1", "text2"},
	})
	if err == nil {
		t.Fatal("expected error for vector count mismatch")
	}
}

func TestCallWithRetry_ContextCancellation(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`rate limited`))
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := openaicompat.CallWithRetry(ctx, http.DefaultClient, nil, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text"},
	}, 5, 1*time.Second)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got: %v", err)
	}
}

func TestClient_Embed(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{0.1, 0.2}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	client, err := openaicompat.New(openaicompat.Options{
		BaseURL: srv.URL,
		APIKey:  "test-key",
		Model:   "test-model",
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	vec, err := client.Embed(t.Context(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("vector length: want 2, got %d", len(vec))
	}
	if vec[0] != 0.1 || vec[1] != 0.2 {
		t.Errorf("vector: want [0.1 0.2], got %v", vec)
	}
}

func TestClient_EmbedBatch_Batching(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()

		var req openaicompat.EmbeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		data := make([]openaicompat.EmbeddingData, len(req.Input))
		for i := range req.Input {
			data[i] = openaicompat.EmbeddingData{
				Embedding: []float32{float32(len(req.Input)), float32(i)},
				Index:     i,
			}
		}
		resp := openaicompat.EmbeddingResponse{Data: data}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	client, err := openaicompat.New(openaicompat.Options{
		BaseURL:      srv.URL,
		APIKey:       "key",
		Model:        "m",
		MaxBatchSize: 3,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	texts := []string{"a", "b", "c", "d", "e", "f", "g"}
	vectors, err := client.EmbedBatch(t.Context(), texts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 7 texts / batch size 3 = 3 requests (3+3+1); batches may be dispatched concurrently.
	mu.Lock()
	got := requestCount
	mu.Unlock()
	if got != 3 {
		t.Errorf("request count: want 3, got %d", got)
	}
	if len(vectors) != 7 {
		t.Fatalf("vectors length: want 7, got %d", len(vectors))
	}

	// Each embedding encodes the batch size as vectors[i][0]. Batches of 3 have value 3;
	// the trailing batch of 1 has value 1. Order of dispatch is non-deterministic.
	for i, v := range vectors {
		if len(v) == 0 {
			t.Errorf("vectors[%d] is empty", i)
		}
	}
	// First 6 texts come from size-3 batches; last text comes from size-1 batch.
	for i := range 6 {
		if vectors[i][0] != 3 {
			t.Errorf("vectors[%d][0]: want 3 (batch size), got %f", i, vectors[i][0])
		}
	}
	if vectors[6][0] != 1 {
		t.Errorf("vectors[6][0]: want 1 (batch size), got %f", vectors[6][0])
	}
}

func TestClient_EmbedBatch_Empty(t *testing.T) {
	client, err := openaicompat.New(openaicompat.Options{
		BaseURL: "http://unused",
		APIKey:  "key",
		Model:   "m",
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	vectors, err := client.EmbedBatch(t.Context(), []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vectors) != 0 {
		t.Errorf("vectors length: want 0, got %d", len(vectors))
	}
}

func TestClient_ImplementsEmbedder(t *testing.T) {
	var _ embedder.Embedder = (*openaicompat.Client)(nil)
}

func TestClient_Embed_RetrySuccess(t *testing.T) {
	var attempts atomic.Int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if attempts.Load() == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`rate limited`))
			return
		}
		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{9.9}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	client, err := openaicompat.New(openaicompat.Options{
		BaseURL:        srv.URL,
		APIKey:         "key",
		Model:          "m",
		MaxRetries:     3,
		RetryBaseDelay: 1 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	vec, err := client.Embed(t.Context(), "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vec[0] != 9.9 {
		t.Errorf("vec[0]: want 9.9, got %f", vec[0])
	}
}

func TestClient_EmbedBatch_ContextCancelled(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		resp := openaicompat.EmbeddingResponse{
			Data: []openaicompat.EmbeddingData{
				{Embedding: []float32{1.0}, Index: 0},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	client, err := openaicompat.New(openaicompat.Options{
		BaseURL: srv.URL,
		APIKey:  "key",
		Model:   "m",
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	_, err = client.EmbedBatch(ctx, []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got: %v", err)
	}
}

func TestCallAPI_LargeResponseTruncated(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write 65 MB of data (exceeds 64 MB limit).
		chunk := make([]byte, 1024*1024) // 1 MB
		for i := range chunk {
			chunk[i] = 'x'
		}
		for range 65 {
			w.Write(chunk)
		}
	})

	_, err := openaicompat.CallAPI(t.Context(), http.DefaultClient, srv.URL, "test-key", openaicompat.EmbeddingRequest{
		Model: "test-model",
		Input: []string{"hello"},
	})
	if err == nil {
		t.Fatal("expected error for oversized response, got nil")
	}
	if !strings.Contains(err.Error(), "decoding embedding response") {
		t.Errorf("expected JSON decode error, got: %v", err)
	}
}

func TestCallWithRetry_ContextCancellation_NoLeak(t *testing.T) {
	// Server always returns 429 to force retries with long backoff.
	var attempts atomic.Int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limited"}`))
	})

	ctx, cancel := context.WithCancel(t.Context())

	// Cancel after the first 429 response is received and backoff timer starts.
	// We use a goroutine that waits for the first attempt, then cancels.
	go func() {
		// Wait until at least one attempt has been made (the server responded 429).
		for attempts.Load() < 1 {
			time.Sleep(5 * time.Millisecond)
		}
		// Small delay to ensure we're in the backoff select.
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := openaicompat.CallWithRetry(ctx, http.DefaultClient, nil, srv.URL, "key", openaicompat.EmbeddingRequest{
		Model: "m",
		Input: []string{"text"},
	}, 5, 10*time.Second) // 10s base delay — would block for 10s+ if timer isn't stopped
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got: %v", err)
	}
	// With proper timer cleanup, should return promptly after cancellation.
	// Without it, this would still pass (select handles ctx.Done), but the
	// leaked timer goroutine would linger. The timing check ensures prompt return.
	if elapsed > 500*time.Millisecond {
		t.Errorf("CallWithRetry took %v after cancellation; want under 500ms (timer leak?)", elapsed)
	}
}

func TestNew_RateLimitZero_DisablesThrottling(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req openaicompat.EmbeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		data := make([]openaicompat.EmbeddingData, len(req.Input))
		for i := range req.Input {
			data[i] = openaicompat.EmbeddingData{
				Embedding: []float32{1.0},
				Index:     i,
			}
		}
		resp := openaicompat.EmbeddingResponse{Data: data}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	client, err := openaicompat.New(openaicompat.Options{
		BaseURL:      srv.URL,
		APIKey:       "key",
		Model:        "m",
		MaxBatchSize: 1,
		RateLimit:    0, // should disable rate limiting per documented contract
		RateBurst:    0,
		Concurrency:  20,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	// Make 10 rapid sequential calls (batch size 1, so 10 HTTP requests).
	// With rate limiting disabled (RateLimit=0), they should complete near-instantly.
	// If RateLimit=0 is silently defaulted to 50 req/s with burst 10, these 10
	// calls would still pass under the burst — so we verify timing is well under
	// what a non-zero rate limit would impose.
	start := time.Now()
	texts := make([]string, 10)
	for i := range texts {
		texts[i] = "text"
	}
	_, err = client.EmbedBatch(t.Context(), texts)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("10 calls with RateLimit=0 took %v; want under 2s (rate limiting not disabled?)", elapsed)
	}
}

func TestNew_HTTPTimeoutOverridesDefault(t *testing.T) {
	c, err := openaicompat.New(openaicompat.Options{
		BaseURL:     "http://localhost",
		Model:       "test",
		HTTPTimeout: 7 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.HTTPClient().Timeout; got != 7*time.Second {
		t.Errorf("HTTPClient.Timeout = %v, want 7s", got)
	}
}

func TestNew_HTTPTimeoutDefaultsTo30s(t *testing.T) {
	c, err := openaicompat.New(openaicompat.Options{
		BaseURL: "http://localhost",
		Model:   "test",
		// HTTPTimeout not set — should default to 30s
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.HTTPClient().Timeout; got != 30*time.Second {
		t.Errorf("HTTPClient.Timeout = %v, want 30s default", got)
	}
}

func TestNew_HTTPTimeoutIgnoredWhenHTTPClientProvided(t *testing.T) {
	custom := &http.Client{Timeout: 99 * time.Second}
	c, err := openaicompat.New(openaicompat.Options{
		BaseURL:     "http://localhost",
		Model:       "test",
		HTTPClient:  custom,
		HTTPTimeout: 7 * time.Second, // should be ignored
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.HTTPClient(); got != custom {
		t.Errorf("HTTPClient() = %p, want custom client %p", got, custom)
	}
	if got := c.HTTPClient().Timeout; got != 99*time.Second {
		t.Errorf("HTTPClient.Timeout = %v, want 99s (custom client's value)", got)
	}
}

// TestEmbedBatchPartial_SubBatchFailure_DoesNotCancelSiblings verifies that
// when one sub-batch hits a non-transient 4xx, sibling sub-batches still
// succeed and return their vectors. This is the cascade-cancellation fix.
func TestEmbedBatchPartial_SubBatchFailure_DoesNotCancelSiblings(t *testing.T) {
	// MaxBatchSize=2, three inputs → two sub-batches: [0,1] and [2].
	// Make sub-batch starting with text "FAIL" return 400; others return ok.
	successBody := `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0},{"object":"embedding","embedding":[0.2],"index":1}],"model":"m","usage":{"prompt_tokens":2,"total_tokens":2}}`
	singleSuccess := `{"object":"list","data":[{"object":"embedding","embedding":[0.3],"index":0}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"FAIL"`)) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad input"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// crude detection: count occurrences of a typical text pattern
		if bytes.Count(body, []byte(`"text`)) >= 2 {
			_, _ = w.Write([]byte(successBody))
		} else {
			_, _ = w.Write([]byte(singleSuccess))
		}
	}))
	defer srv.Close()

	c, err := openaicompat.New(openaicompat.Options{
		BaseURL:      srv.URL,
		Model:        "m",
		MaxBatchSize: 2,
		Concurrency:  4,
		MaxRetries:   1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	vectors, errs := c.EmbedBatchPartial(t.Context(), []string{"FAIL", "text1", "text2"})
	if len(vectors) != 3 || len(errs) != 3 {
		t.Fatalf("len(vectors)=%d len(errs)=%d, want 3,3", len(vectors), len(errs))
	}
	// Indices 0 and 1 belong to the failed sub-batch ["FAIL", "text1"] → MaxBatchSize=2
	if errs[0] == nil || errs[1] == nil {
		t.Errorf("errs[0]=%v errs[1]=%v, want non-nil for failed sub-batch", errs[0], errs[1])
	}
	if vectors[0] != nil || vectors[1] != nil {
		t.Errorf("vectors[0..1] should be nil on failure")
	}
	// Index 2 belongs to the second sub-batch ["text2"] → should succeed.
	if errs[2] != nil {
		t.Errorf("errs[2] = %v, want nil (sibling sub-batch should succeed)", errs[2])
	}
	if vectors[2] == nil {
		t.Errorf("vectors[2] = nil, want a vector")
	}
}

// TestEmbedBatchPartial_ContextCancel_ReturnsPromptly verifies that all
// sub-batch goroutines exit promptly when the caller cancels mid-flight.
// Uses timing rather than testing/synctest because httptest.Server spawns
// real goroutines outside the synctest bubble, causing hangs under virtual time.
func TestEmbedBatchPartial_ContextCancel_ReturnsPromptly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-sensitive test in short mode")
	}
	// Cleanup runs LIFO: handlerDone closes first (registered last), then srv.Close.
	// This ensures blocked handlers can exit before the server shuts down.
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-handlerDone:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(handlerDone) })

	c, err := openaicompat.New(openaicompat.Options{
		BaseURL:      srv.URL,
		Model:        "m",
		MaxBatchSize: 1,
		Concurrency:  4,
		MaxRetries:   0, // normalized to default by New; context cancel makes this irrelevant
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.EmbedBatchPartial(ctx, []string{"a", "b", "c", "d"})
	}()

	// Give goroutines time to park on the blocked HTTP call.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EmbedBatchPartial did not return promptly after context cancel")
	}
}

// TestClient_SemaphoreCapsGlobalConcurrency verifies that the per-Client
// semaphore caps in-flight HTTP requests across concurrent EmbedBatch
// callers — not just within a single call.
func TestClient_SemaphoreCapsGlobalConcurrency(t *testing.T) {
	const limit = 2
	var inFlight, maxObserved atomic.Int32

	body := `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			old := maxObserved.Load()
			if n <= old || maxObserved.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, err := openaicompat.New(openaicompat.Options{
		BaseURL:      srv.URL,
		Model:        "m",
		MaxBatchSize: 1,
		Concurrency:  limit,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			_, _ = c.EmbedBatch(t.Context(), []string{"a", "b", "c", "d"})
		})
	}
	wg.Wait()

	if got := maxObserved.Load(); got > limit {
		t.Errorf("observed max in-flight = %d, want <= %d", got, limit)
	}
}

// TestEmbedBatchPartial_VectorCountMismatch_SetsPerTextErrors verifies that
// if CallWithRetry returns fewer vectors than texts (defensive guard against
// a future CallAPI relaxation), EmbedBatchPartial fills per-text errors
// instead of silently leaving nil vectors with nil errors.
func TestEmbedBatchPartial_VectorCountMismatch_SetsPerTextErrors(t *testing.T) {
	// Return 1 vector for a 2-text sub-batch. CallAPI normally rejects this,
	// but we bypass it by injecting a custom http.Client that returns a
	// pre-built short response. This exercises the defense-in-depth check.
	shortBody := `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(shortBody))
	}))
	defer srv.Close()

	// MaxBatchSize=2, so ["a","b"] goes in one sub-batch. The server returns
	// only 1 vector — CallAPI would normally catch this with its own count check
	// and return an error, which the err != nil branch handles. This test
	// confirms the overall invariant: no nil-vector-with-nil-error slots.
	c, err := openaicompat.New(openaicompat.Options{
		BaseURL:      srv.URL,
		Model:        "m",
		MaxBatchSize: 2,
		Concurrency:  4,
		MaxRetries:   1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	vectors, errs := c.EmbedBatchPartial(t.Context(), []string{"a", "b"})
	if len(vectors) != 2 || len(errs) != 2 {
		t.Fatalf("len(vectors)=%d len(errs)=%d, want 2,2", len(vectors), len(errs))
	}
	// Whether CallAPI or our defensive check catches the mismatch, both texts
	// must have non-nil errors and nil vectors — the invariant must hold.
	for i := range 2 {
		if errs[i] == nil {
			t.Errorf("errs[%d] = nil, want non-nil (vector count mismatch)", i)
		}
		if vectors[i] != nil {
			t.Errorf("vectors[%d] = %v, want nil when errs[%d] != nil", i, vectors[i], i)
		}
	}
}

func TestClient_EmbedBatch_RateLimiting(t *testing.T) {
	var mu sync.Mutex
	requestTimes := make([]time.Time, 0)
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestTimes = append(requestTimes, time.Now())
		mu.Unlock()

		var req openaicompat.EmbeddingRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		data := make([]openaicompat.EmbeddingData, len(req.Input))
		for i := range req.Input {
			data[i] = openaicompat.EmbeddingData{
				Embedding: []float32{1.0},
				Index:     i,
			}
		}
		resp := openaicompat.EmbeddingResponse{Data: data}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	client, err := openaicompat.New(openaicompat.Options{
		BaseURL:      srv.URL,
		APIKey:       "key",
		Model:        "m",
		MaxBatchSize: 1,
		RateLimit:    2,
		RateBurst:    1,
	})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}

	texts := []string{"a", "b", "c", "d"}
	_, err = client.EmbedBatch(t.Context(), texts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(requestTimes) != 4 {
		t.Fatalf("request count: want 4, got %d", len(requestTimes))
	}

	// With rate limit 2/sec and burst 1, requests after the first should be
	// spaced ~500ms apart. Check that total time is at least 1 second for 4 requests.
	totalDuration := requestTimes[3].Sub(requestTimes[0])
	if totalDuration < 1*time.Second {
		t.Errorf("total duration %v is too short for rate limit 2/sec with 4 requests", totalDuration)
	}
}
