package openaicompat

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"time"
)

// isTransientTransportErr reports whether err is a known-transient network transport
// error safe to retry. It returns false for context errors, DNS failures, TLS
// certificate verification failures, and unknown error types.
// errors.As traverses *url.Error wrappers automatically, so no manual unwrapping
// is needed.
func isTransientTransportErr(err error) bool {
	if err == nil {
		return false
	}
	// Context errors are never transient.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Fatal: DNS resolution failure.
	// Must be checked before *net.OpError: a DNS failure chain is
	// *url.Error → *net.OpError → *net.DNSError; the OpErr guard below would
	// otherwise misclassify it as transient.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return false
	}
	// Fatal: TLS certificate verification failure (wraps x509 errors).
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return false
	}
	// Transient: connection closed mid-stream.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Transient TLS peer alerts: clean server-side close (0), MAC failure on a
	// reused connection (20), and server internal error (80).
	// Certificate and handshake alerts are fatal.
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		switch alertErr {
		case 0, 20, 80: // close_notify, bad_record_mac, internal_error
			return true
		}
		return false
	}
	// Transient: network-level errors (connection reset, broken pipe, ETIMEDOUT).
	// Reached only after the DNS guard above, so *net.DNSError is excluded.
	// Note: this also matches ECONNREFUSED — the spec treats all *net.OpError
	// as transient; tighten here if ECONNREFUSED should be fatal.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	return false
}

// CallWithRetry calls CallAPI with exponential backoff + jitter on retryable errors.
// It retries on HTTP 429, 5xx, and known-transient transport errors (EOF, connection
// reset, TLS bad_record_mac, etc.). It does not retry on 4xx (non-429), fatal
// transport errors (DNS, certificate), or context cancellation.
func CallWithRetry(ctx context.Context, client *http.Client, baseURL, apiKey string, req EmbeddingRequest, maxRetries int, baseDelay time.Duration) ([][]float32, error) {
	var lastErr error
	for attempt := range maxRetries + 1 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		vectors, err := CallAPI(ctx, client, baseURL, apiKey, req)
		if err == nil {
			return vectors, nil
		}

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			if !isTransientTransportErr(err) {
				return nil, err
			}
			// Transient transport error — fall through to shared backoff below.
		} else if apiErr.StatusCode != http.StatusTooManyRequests && apiErr.StatusCode < 500 {
			return nil, fmt.Errorf("%w: %w", ErrAPIError, err)
		}

		lastErr = err
		if attempt == maxRetries {
			break
		}

		delay := baseDelay * time.Duration(1<<attempt)
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		var jitter time.Duration
		if delay/2 > 0 {
			jitter = time.Duration(rand.Int64N(int64(delay / 2)))
		}

		timer := time.NewTimer(delay + jitter)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}

	if lastErr != nil {
		var apiErr *APIError
		if errors.As(lastErr, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: %w", ErrRateLimited, lastErr)
		}
	}
	return nil, fmt.Errorf("embedding API: retries exhausted: %w", lastErr)
}
