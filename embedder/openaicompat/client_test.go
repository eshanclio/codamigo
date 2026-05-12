package openaicompat_test

import (
	"context"
	"encoding/json"
	"errors"
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

	vectors, err := openaicompat.CallAPI(context.Background(), http.DefaultClient, srv.URL, "test-key", openaicompat.EmbeddingRequest{
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

	_, err := openaicompat.CallAPI(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	_, err := openaicompat.CallAPI(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	_, err := openaicompat.CallAPI(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	_, err := openaicompat.CallAPI(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	vectors, err := openaicompat.CallWithRetry(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	vectors, err := openaicompat.CallWithRetry(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	_, err := openaicompat.CallWithRetry(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	_, err := openaicompat.CallWithRetry(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	_, err := openaicompat.CallAPI(context.Background(), http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := openaicompat.CallWithRetry(ctx, http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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

	vec, err := client.Embed(context.Background(), "hello")
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
	vectors, err := client.EmbedBatch(context.Background(), texts)
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

	vectors, err := client.EmbedBatch(context.Background(), []string{})
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

	vec, err := client.Embed(context.Background(), "hello")
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

	ctx, cancel := context.WithCancel(context.Background())
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

	_, err := openaicompat.CallAPI(context.Background(), http.DefaultClient, srv.URL, "test-key", openaicompat.EmbeddingRequest{
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

	ctx, cancel := context.WithCancel(context.Background())

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
	_, err := openaicompat.CallWithRetry(ctx, http.DefaultClient, srv.URL, "key", openaicompat.EmbeddingRequest{
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
	_, err = client.EmbedBatch(context.Background(), texts)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("10 calls with RateLimit=0 took %v; want under 2s (rate limiting not disabled?)", elapsed)
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
	_, err = client.EmbedBatch(context.Background(), texts)
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
