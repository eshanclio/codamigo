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
	"syscall"
	"testing"
	"time"
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
		context.Background(),
		client,
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
		context.Background(),
		client,
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
		context.Background(),
		client,
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
