package openaicompat

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
)

// roundTripFunc allows using a plain function as an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestIsTransientTransportErr(t *testing.T) {
	dnsErr := &net.DNSError{Name: "api.openai.com", Err: "no such host"}
	certErr := &tls.CertificateVerificationError{Err: errors.New("certificate signed by unknown authority")}
	opErr := &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
		{"dns error", dnsErr, false},
		{"tls cert verification", certErr, false},
		{"tls alert handshake_failure=40", tls.AlertError(40), false},
		{"tls alert bad_certificate=42", tls.AlertError(42), false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"tls alert close_notify=0", tls.AlertError(0), true},
		{"tls alert bad_record_mac=20", tls.AlertError(20), true},
		{"tls alert internal_error=80", tls.AlertError(80), true},
		{"net.OpError ECONNRESET", opErr, true},
		// Wrapped in *url.Error — errors.As must traverse the wrapper.
		{"url.Error wrapping ErrUnexpectedEOF", &url.Error{Op: "Post", URL: "u", Err: io.ErrUnexpectedEOF}, true},
		{"url.Error wrapping DNSError", &url.Error{Op: "Post", URL: "u", Err: dnsErr}, false},
		{"url.Error wrapping context.Canceled", &url.Error{Op: "Post", URL: "u", Err: context.Canceled}, false},
		{"url.Error wrapping tls.AlertError(20)", &url.Error{Op: "Post", URL: "u", Err: tls.AlertError(20)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientTransportErr(tc.err); got != tc.want {
				t.Errorf("isTransientTransportErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestCallWithRetry_TransientTransportError_IsRetried(t *testing.T) {
	const wantCalls = 3 // 2 transient failures then success
	callCount := 0

	successBody := `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			if callCount < wantCalls {
				return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: io.ErrUnexpectedEOF}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(successBody)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}),
	}

	vectors, err := CallWithRetry(
		t.Context(),
		client,
		nil,
		"http://localhost",
		"test-key",
		EmbeddingRequest{Model: "m", Input: []string{"hello"}},
		3,
		time.Millisecond,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vectors) != 1 {
		t.Fatalf("want 1 vector, got %d", len(vectors))
	}
	if callCount != wantCalls {
		t.Fatalf("want %d calls (%d retries), got %d", wantCalls, wantCalls-1, callCount)
	}
}

func TestCallWithRetry_FatalTransportError_NotRetried(t *testing.T) {
	callCount := 0
	dnsErr := &net.DNSError{Name: "api.openai.com", Err: "no such host"}

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: dnsErr}
		}),
	}

	_, err := CallWithRetry(
		t.Context(),
		client,
		nil,
		"http://localhost",
		"test-key",
		EmbeddingRequest{Model: "m", Input: []string{"hello"}},
		3,
		time.Millisecond,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if callCount != 1 {
		t.Fatalf("fatal transport error must not be retried: want 1 call, got %d", callCount)
	}
}

// TestCallWithRetry_HTTPClientTimeout_IsRetried verifies that an http.Client.Timeout
// expiry is retried when the caller's context is still live. This is the key
// distinction: http.Client.Timeout surfaces as context.DeadlineExceeded (a *url.Error
// wrapping it), but it is a transient server-side stall, not a caller cancellation.
func TestCallWithRetry_HTTPClientTimeout_IsRetried(t *testing.T) {
	const wantCalls = 3 // 2 timeouts then success
	callCount := 0

	successBody := `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			if callCount < wantCalls {
				// Simulate http.Client.Timeout: url.Error wrapping context.DeadlineExceeded.
				return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: context.DeadlineExceeded}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(successBody)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}),
	}

	// Caller context is live — deadline came from the http client, not from us.
	vectors, err := CallWithRetry(
		t.Context(),
		client,
		nil,
		"http://localhost",
		"test-key",
		EmbeddingRequest{Model: "m", Input: []string{"hello"}},
		3,
		time.Millisecond,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vectors) != 1 {
		t.Fatalf("want 1 vector, got %d", len(vectors))
	}
	if callCount != wantCalls {
		t.Fatalf("want %d calls (%d retries), got %d", wantCalls, wantCalls-1, callCount)
	}
}

// TestCallWithRetry_CallerContextDeadline_NotRetried verifies that when the caller's
// own context expires, the error is NOT retried even though it also surfaces as
// context.DeadlineExceeded.
func TestCallWithRetry_CallerContextDeadline_NotRetried(t *testing.T) {
	callCount := 0

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: context.DeadlineExceeded}
		}),
	}

	// Caller context is already expired before the first attempt.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := CallWithRetry(
		ctx,
		client,
		nil,
		"http://localhost",
		"test-key",
		EmbeddingRequest{Model: "m", Input: []string{"hello"}},
		3,
		time.Millisecond,
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
	}
	// The pre-flight ctx.Err() check at the top of the loop catches it before the
	// first HTTP call, so callCount must be 0.
	if callCount != 0 {
		t.Fatalf("expired caller context must not trigger any HTTP calls, got %d", callCount)
	}
}

func TestCallWithRetry_TransientTransportError_ExhaustsRetries(t *testing.T) {
	const maxRetries = 2
	callCount := 0

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: io.ErrUnexpectedEOF}
		}),
	}

	_, err := CallWithRetry(
		t.Context(),
		client,
		nil,
		"http://localhost",
		"test-key",
		EmbeddingRequest{Model: "m", Input: []string{"hello"}},
		maxRetries,
		time.Millisecond,
	)
	if err == nil {
		t.Fatal("expected error after exhausted retries, got nil")
	}
	wantCalls := maxRetries + 1 // initial attempt + maxRetries retries
	if callCount != wantCalls {
		t.Fatalf("want %d calls, got %d", wantCalls, callCount)
	}
}

// TestCallWithRetry_ReleasesSemaphoreAcrossBackoff verifies that when an
// attempt fails transiently and CallWithRetry sleeps for backoff, the
// semaphore slot is released so siblings can acquire it.
func TestCallWithRetry_ReleasesSemaphoreAcrossBackoff(t *testing.T) {
	sem := semaphore.NewWeighted(1)

	// firstCallFailed is signalled by the roundtripper right before it returns
	// the transient error, allowing the concurrent goroutine to wait until after
	// the first failure. Buffered so the roundtripper never blocks.
	firstCallFailed := make(chan struct{}, 1)

	// Roundtripper that fails the first attempt with io.ErrUnexpectedEOF
	// (transient), succeeds the second.
	var calls atomic.Int32
	successBody := `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"m","usage":{"prompt_tokens":1,"total_tokens":1}}`
	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			n := calls.Add(1)
			if n == 1 {
				firstCallFailed <- struct{}{} // signal: returning transient error now
				return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: io.ErrUnexpectedEOF}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(successBody)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}),
	}

	// Concurrent acquirer must be able to grab the slot during backoff.
	acquired := make(chan struct{})
	go func() {
		// Wait until the first call has failed (roundtripper returned the error).
		// Then sleep briefly to let CallWithRetry call sem.Release after CallAPI returns.
		<-firstCallFailed
		time.Sleep(5 * time.Millisecond)
		// Try to acquire with a short timeout; we expect to succeed because
		// CallWithRetry releases before sleeping.
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		if err := sem.Acquire(ctx, 1); err != nil {
			return
		}
		defer sem.Release(1)
		close(acquired)
	}()

	vectors, err := CallWithRetry(
		t.Context(), client, sem,
		"http://localhost", "k",
		EmbeddingRequest{Model: "m", Input: []string{"hello"}},
		2,
		50*time.Millisecond, // baseDelay — first backoff ~50ms + jitter
	)
	if err != nil {
		t.Fatalf("CallWithRetry: %v", err)
	}
	if len(vectors) != 1 {
		t.Fatalf("vectors len = %d, want 1", len(vectors))
	}
	select {
	case <-acquired:
	case <-time.After(1 * time.Second):
		t.Fatal("concurrent acquirer was unable to grab the semaphore during backoff — slot held across sleep")
	}
}
