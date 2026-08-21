package tui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/javanhut/ollama_code/api"
)

func TestStreamRetryDelayBackoffBounds(t *testing.T) {
	err := errors.New("read: connection reset by peer")
	for attempt := 1; attempt <= 8; attempt++ {
		want := streamRetryBaseDelay << (attempt - 1)
		if want > streamRetryMaxDelay {
			want = streamRetryMaxDelay
		}
		lo, hi := time.Duration(float64(want)*0.74), time.Duration(float64(want)*1.26)
		for range 50 {
			got := streamRetryDelay(attempt, err)
			if got < lo || got > hi {
				t.Fatalf("attempt %d: delay %v outside jitter bounds [%v, %v]", attempt, got, lo, hi)
			}
		}
	}
}

// retryAfterError runs one request against a server that always answers 429
// with the given Retry-After, returning the surfaced error.
func retryAfterError(t *testing.T, retryAfter string) error {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", retryAfter)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"too many requests"}`))
	}))
	defer server.Close()
	host := api.OllamaHost{}
	host.SetURI(server.URL)
	_, err := host.ChatOnce(context.Background(), api.ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected the 429 to surface")
	}
	return err
}

func TestStreamRetryDelayHonorsRetryAfter(t *testing.T) {
	// The provider's ask wins even above our own 30s cap.
	err := retryAfterError(t, "45")
	if got := streamRetryDelay(1, err); got != 45*time.Second {
		t.Fatalf("Retry-After 45: delay = %v want 45s", got)
	}
	// ...but not past the sanity ceiling.
	err = retryAfterError(t, "3600")
	if got := streamRetryDelay(1, err); got != streamRetryProviderMax {
		t.Fatalf("Retry-After 3600: delay = %v want the %v cap", got, streamRetryProviderMax)
	}
}

func TestStreamRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true}, // unknown errors keep the historical benefit of the budget
		{"connection reset", errors.New("read: connection reset by peer"), true},
		{"unknown transport", errors.New("stream stalled"), true},
		{"429", errors.New(`unexpected status code: 429: {"error":"quota"}`), true},
		{"500", errors.New(`unexpected status code: 500: internal error`), true},
		{"503", errors.New(`unexpected status code: 503: no runner`), true},
		{"400 with transient body", errors.New(`unexpected status code: 400: {"error":"model overloaded, try again"}`), true},
		{"400 bad request", errors.New(`unexpected status code: 400: {"error":"bad request"}`), false},
		{"401", errors.New(`unexpected status code: 401: {"error":"unauthorized"}`), false},
		{"403", errors.New(`unexpected status code: 403: {"error":"forbidden"}`), false},
		{"404", errors.New(`unexpected status code: 404: {"error":"model not found"}`), false},
		{"context overflow", errors.New(`unexpected status code: 400: {"error":{"code":"context_length_exceeded"}}`), false},
		{"format rejection", errors.New(`unexpected status code: 400: {"error": "JSON schema conversion failed: Unrecognized schema"}`), false},
		{"memory failure", errors.New(`unexpected status code: 500: cudaMalloc failed: out of memory`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamRetryable(tc.err); got != tc.want {
				t.Fatalf("streamRetryable(%v) = %v want %v", tc.err, got, tc.want)
			}
		})
	}
}
