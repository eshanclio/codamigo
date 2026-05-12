package openaicompat

import "errors"

// ErrRateLimited is returned when the API returns 429 and all retries are exhausted.
var ErrRateLimited = errors.New("embedding API: rate limited, retries exhausted")

// ErrAPIError is returned when the API returns a non-transient 4xx error.
// Callers can use errors.Is to distinguish API errors from network or context errors.
var ErrAPIError = errors.New("embedding API error")
