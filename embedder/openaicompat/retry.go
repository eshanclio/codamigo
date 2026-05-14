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

	"golang.org/x/sync/semaphore"
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
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return false
	}
	// Fatal: TLS certificate verification failure (wraps x509 errors).
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return false
	}
	// Transient: connection closed mid-stream.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// Transient TLS peer alerts: clean server-side close (0), MAC failure on a
	// reused connection (20), and server internal error (80).
	// Certificate and handshake alerts are fatal.
	if alertErr, ok := errors.AsType[tls.AlertError](err); ok {
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
	if _, ok := errors.AsType[*net.OpError](err); ok {
		return true
	}
	return false
}

// CallWithRetry calls CallAPI with exponential backoff + jitter on retryable errors.
// It retries on HTTP 429, 5xx, known-transient transport errors (EOF, connection
// reset, TLS bad_record_mac, etc.), and http.Client.Timeout expiry (which surfaces
// as context.DeadlineExceeded but is distinguishable because the caller's ctx is
// still live). It does not retry on 4xx (non-429), fatal transport errors (DNS,
// certificate), or cancellation/expiry of the caller's context.
//
// sem gates each HTTP attempt: acquired before CallAPI, released immediately after
// (before any backoff sleep) so slots are available to sibling goroutines during
// retry waits. Pass nil to disable gating.
func CallWithRetry(
	ctx context.Context,
	client *http.Client,
	sem *semaphore.Weighted,
	baseURL, apiKey string,
	req EmbeddingRequest,
	maxRetries int,
	baseDelay time.Duration,
) ([][]float32, error) {
	var lastErr error
	for attempt := range maxRetries + 1 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if sem != nil {
			if err := sem.Acquire(ctx, 1); err != nil {
				return nil, err
			}
		}
		vectors, err := CallAPI(ctx, client, baseURL, apiKey, req)
		if sem != nil {
			sem.Release(1)
		}

		if err == nil {
			return vectors, nil
		}

		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			// http.Client.Timeout fires as context.DeadlineExceeded (wrapped in
			// *url.Error), indistinguishable from a caller cancellation by error
			// type alone. Distinguish by checking whether the caller's ctx is
			// still live: if it is, the deadline came from the http client, which
			// is a transient server-side stall and should be retried.
			isCtxErr := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
			if isCtxErr {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				// http.Client.Timeout — fall through to retry/backoff.
			} else if !isTransientTransportErr(err) {
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

		// Semaphore is already released above — backoff sleep does not hold any slot.
		timer := time.NewTimer(delay + jitter)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	}

	if lastErr != nil {
		if apiErr, ok := errors.AsType[*APIError](lastErr); ok && apiErr.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: %w", ErrRateLimited, lastErr)
		}
	}
	return nil, fmt.Errorf("embedding API: retries exhausted: %w", lastErr)
}
