// Package openaicompat provides an OpenAI-compatible embedding API client
// that implements [embedder.Embedder].
//
// It works with any provider that follows the OpenAI /v1/embeddings API
// shape: OpenAI, Voyage AI, Azure OpenAI, Ollama, LM Studio, and others.
// The client handles request batching, proactive rate limiting via a token
// bucket, and exponential backoff with jitter on 429 and 5xx responses.
// Every blocking point respects context cancellation.
package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// Options configures an OpenAI-compatible embedding client.
type Options struct {
	BaseURL        string        // base URL of the embedding API, e.g. "https://api.openai.com/v1"
	APIKey         string        // API key sent in the Authorization header
	Model          string        // embedding model name, e.g. "text-embedding-3-small"
	Dimensions     int           // embedding vector dimensions; 0 uses the model default
	InputType      string        // optional provider-specific input type, e.g. "document" or "query" for Voyage AI
	MaxBatchSize   int           // maximum texts per API call; 0 defaults to 64
	MaxRetries     int           // maximum retry attempts on 429 and 5xx errors
	RateLimit      float64       // sustained requests per second; 0 disables rate limiting
	RateBurst      int           // maximum burst above the sustained rate
	RetryBaseDelay time.Duration // initial backoff delay before the first retry; doubles each attempt
	HTTPClient     *http.Client  // optional custom HTTP client; nil uses a pooled client
	// HTTPTimeout is applied only when HTTPClient is nil. Default 30s
	// (preserved for direct API users); main.go overrides this to a higher
	// value derived from config.EmbeddingHTTPTimeout.
	HTTPTimeout time.Duration
	Concurrency int // concurrent sub-batch HTTP requests; 0 defaults to 8
}

// Client is an OpenAI-compatible embedding API client that implements embedder.Embedder.
type Client struct {
	httpClient     *http.Client
	baseURL        string
	apiKey         string
	model          string
	dimensions     int
	inputType      string
	maxBatchSize   int
	maxRetries     int
	retryBaseDelay time.Duration
	concurrency    int
	limiter        *rate.Limiter
	sem            *semaphore.Weighted
}

// New constructs a Client from the given options, validating required fields.
// Zero-value numeric fields use built-in defaults (64 texts per batch, 5 retries,
// 500ms base delay, 8 concurrent sub-batches).
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("baseURL must not be empty")
	}
	if opts.Model == "" {
		return nil, errors.New("model must not be empty")
	}

	if opts.MaxBatchSize <= 0 {
		opts.MaxBatchSize = 64
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 5
	}
	if opts.RetryBaseDelay <= 0 {
		opts.RetryBaseDelay = 500 * time.Millisecond
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.HTTPClient == nil {
		timeout := opts.HTTPTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		opts.HTTPClient = &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: opts.Concurrency,
			},
		}
	}

	var limiter *rate.Limiter
	if opts.RateLimit > 0 {
		if opts.RateBurst <= 0 {
			opts.RateBurst = 10
		}
		limiter = rate.NewLimiter(rate.Limit(opts.RateLimit), opts.RateBurst)
	} else {
		limiter = rate.NewLimiter(rate.Inf, 0)
	}

	sem := semaphore.NewWeighted(int64(opts.Concurrency))

	return &Client{
		httpClient:     opts.HTTPClient,
		baseURL:        opts.BaseURL,
		apiKey:         opts.APIKey,
		model:          opts.Model,
		dimensions:     opts.Dimensions,
		inputType:      opts.InputType,
		maxBatchSize:   opts.MaxBatchSize,
		maxRetries:     opts.MaxRetries,
		retryBaseDelay: opts.RetryBaseDelay,
		concurrency:    opts.Concurrency,
		limiter:        limiter,
		sem:            sem,
	}, nil
}

// HTTPClient returns the underlying *http.Client. Exposed for tests and for
// callers that need to tune transport behavior post-construction.
func (c *Client) HTTPClient() *http.Client {
	return c.httpClient
}

// Embed embeds a single text string and returns its float32 vector.
// It respects ctx cancellation at the rate-limiter wait and HTTP request.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := c.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, fmt.Errorf("embedding API: no vectors returned")
	}
	return vectors[0], nil
}

// EmbedBatchPartial embeds texts in sub-batches with per-text failure
// isolation. Invariants: len(vectors)==len(errs)==len(texts);
// vectors[i] is nil iff errs[i] != nil.
func (c *Client) EmbedBatchPartial(ctx context.Context, texts []string) ([][]float32, []error) {
	vectors := make([][]float32, len(texts))
	errs := make([]error, len(texts))
	if len(texts) == 0 {
		return vectors, errs
	}

	totalBatches := (len(texts) + c.maxBatchSize - 1) / c.maxBatchSize
	var wg sync.WaitGroup

	for i := range totalBatches {
		start := i * c.maxBatchSize
		end := min(start+c.maxBatchSize, len(texts))

		wg.Go(func() {
			if err := c.limiter.Wait(ctx); err != nil {
				for k := start; k < end; k++ {
					errs[k] = err
				}
				return
			}

			req := EmbeddingRequest{
				Model:      c.model,
				Input:      texts[start:end],
				Dimensions: c.dimensions,
				InputType:  c.inputType,
			}
			batchVectors, err := CallWithRetry(ctx, c.httpClient, c.sem, c.baseURL, c.apiKey, req, c.maxRetries, c.retryBaseDelay)
			if err != nil {
				for k := start; k < end; k++ {
					errs[k] = fmt.Errorf("embedding batch %d/%d: %w", i+1, totalBatches, err)
				}
				return
			}
			if len(batchVectors) != end-start {
				err = fmt.Errorf("embedding batch %d/%d: got %d vectors for %d texts", i+1, totalBatches, len(batchVectors), end-start)
				for k := start; k < end; k++ {
					errs[k] = err
				}
				return
			}
			for k := range batchVectors {
				vectors[start+k] = batchVectors[k]
			}
		})
	}
	wg.Wait()
	return vectors, errs
}

// EmbedBatch is the all-or-nothing wrapper around EmbedBatchPartial.
// It returns errors.Join'd failure on any sub-batch error, preserving
// backward-compatible semantics for callers that don't need per-text results.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	vectors, errs := c.EmbedBatchPartial(ctx, texts)
	if joined := errors.Join(errs...); joined != nil {
		return nil, joined
	}
	return vectors, nil
}
