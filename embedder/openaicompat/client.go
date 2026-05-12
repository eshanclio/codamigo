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
	"time"

	"golang.org/x/sync/errgroup"
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
	Concurrency    int           // concurrent sub-batch HTTP requests; 0 defaults to 8
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
		opts.HTTPClient = &http.Client{
			Timeout: 30 * time.Second,
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
	}, nil
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

// EmbedBatch embeds multiple texts, splitting into sub-batches when
// len(texts) > MaxBatchSize. Sub-batches are dispatched concurrently up to
// the configured Concurrency limit. Returns one vector per input in the same
// order. Respects ctx cancellation throughout.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}

	totalBatches := (len(texts) + c.maxBatchSize - 1) / c.maxBatchSize
	batchResults := make([][][]float32, totalBatches)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.concurrency)

	for i := range totalBatches {
		g.Go(func() error {
			if err := c.limiter.Wait(gctx); err != nil {
				return err
			}

			start := i * c.maxBatchSize
			end := min(start+c.maxBatchSize, len(texts))
			batch := texts[start:end]

			req := EmbeddingRequest{
				Model:      c.model,
				Input:      batch,
				Dimensions: c.dimensions,
				InputType:  c.inputType,
			}

			vectors, err := CallWithRetry(gctx, c.httpClient, c.baseURL, c.apiKey, req, c.maxRetries, c.retryBaseDelay)
			if err != nil {
				return fmt.Errorf("embedding batch %d/%d: %w", i+1, totalBatches, err)
			}

			batchResults[i] = vectors
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	result := make([][]float32, 0, len(texts))
	for _, br := range batchResults {
		result = append(result, br...)
	}
	return result, nil
}
