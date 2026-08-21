package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name   string
		header http.Header
		want   time.Duration
		ok     bool
	}{
		{"seconds", http.Header{"Retry-After": {"7"}}, 7 * time.Second, true},
		{"zero seconds", http.Header{"Retry-After": {"0"}}, 0, true},
		{"milliseconds", http.Header{"Retry-After-Ms": {"1500"}}, 1500 * time.Millisecond, true},
		{"ms wins over seconds", http.Header{"Retry-After-Ms": {"250"}, "Retry-After": {"9"}}, 250 * time.Millisecond, true},
		{"invalid ms falls back to seconds", http.Header{"Retry-After-Ms": {"nope"}, "Retry-After": {"3"}}, 3 * time.Second, true},
		{"garbage", http.Header{"Retry-After": {"soon"}}, 0, false},
		{"missing", http.Header{}, 0, false},
		{"nil header", nil, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.header)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("parseRetryAfter(%v) = %v, %v want %v, %v", tc.header, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseRetryAfterHTTPDate(t *testing.T) {
	future := time.Now().Add(3 * time.Second).UTC().Format(http.TimeFormat)
	got, ok := parseRetryAfter(http.Header{"Retry-After": {future}})
	if !ok {
		t.Fatalf("HTTP-date %q not accepted", future)
	}
	if got <= 0 || got > 3*time.Second {
		t.Fatalf("HTTP-date delay = %v, want within (0, 3s]", got)
	}

	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	got, ok = parseRetryAfter(http.Header{"Retry-After": {past}})
	if !ok || got != 0 {
		t.Fatalf("past HTTP-date = %v, %v want 0, true (retry now)", got, ok)
	}
}

func TestStatusErrorStringFormatUnchanged(t *testing.T) {
	withBody := statusError(500, []byte(`{"error":"boom"}`), nil)
	if got, want := withBody.Error(), `unexpected status code: 500: {"error":"boom"}`; got != want {
		t.Fatalf("Error() = %q want %q", got, want)
	}
	bare := statusError(404, nil, nil)
	if got, want := bare.Error(), "unexpected status code: 404"; got != want {
		t.Fatalf("Error() = %q want %q", got, want)
	}
}

func TestStatusErrorCarriesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"temporarily unavailable"}`))
	}))
	defer server.Close()
	host := OllamaHost{uri: server.URL}

	_, err := host.ChatOnce(context.Background(), ChatRequest{Model: "m"})
	if err == nil {
		t.Fatal("expected the 503 to surface")
	}
	d, ok := RetryAfterDelay(err)
	if !ok || d != 7*time.Second {
		t.Fatalf("RetryAfterDelay(%v) = %v, %v want 7s, true", err, d, ok)
	}
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 503 {
		t.Fatalf("errors.As(*StatusError) failed or wrong code: %v", err)
	}
}

func TestRetryAfterDelayIgnoresOtherErrors(t *testing.T) {
	if d, ok := RetryAfterDelay(errors.New("connection reset by peer")); ok || d != 0 {
		t.Fatalf("RetryAfterDelay on plain error = %v, %v want 0, false", d, ok)
	}
	if d, ok := RetryAfterDelay(nil); ok || d != 0 {
		t.Fatalf("RetryAfterDelay(nil) = %v, %v want 0, false", d, ok)
	}
}

func TestStatusCodeOf(t *testing.T) {
	if got := StatusCodeOf(statusError(429, []byte("slow down"), nil)); got != 429 {
		t.Fatalf("StatusCodeOf(StatusError) = %d want 429", got)
	}
	// Errors from paths still built with fmt.Errorf classify by their string.
	if got := StatusCodeOf(fmt.Errorf("unexpected status code: 403: forbidden")); got != 403 {
		t.Fatalf("StatusCodeOf(string form) = %d want 403", got)
	}
	if got := StatusCodeOf(errors.New("no status here")); got != 0 {
		t.Fatalf("StatusCodeOf(no status) = %d want 0", got)
	}
	if got := StatusCodeOf(nil); got != 0 {
		t.Fatalf("StatusCodeOf(nil) = %d want 0", got)
	}
}

func TestIsTransientError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429 status", statusError(429, []byte(`{"error":"quota"}`), nil), true},
		{"503 status", statusError(503, []byte(`{"error":"nope"}`), nil), true},
		{"500 status", statusError(500, []byte(`internal error`), nil), true},
		{"overloaded body on 400", statusError(400, []byte(`{"error":"model overloaded"}`), nil), true},
		{"rate limit body on 400", statusError(400, []byte(`rate_limit exceeded`), nil), true},
		{"too many requests body", statusError(400, []byte(`Too Many Requests`), nil), true},
		{"connection reset", errors.New("read: connection reset by peer"), true},
		{"connection refused", errors.New("dial tcp: connection refused"), true},
		{"EOF", errors.New("unexpected EOF"), true},
		{"timeout", errors.New("context deadline exceeded: timeout"), true},
		{"timed out", errors.New("i/o timed out"), true},
		{"temporarily unavailable", statusError(400, []byte(`service temporarily unavailable`), nil), true},
		{"bad gateway text", errors.New("502 Bad Gateway"), true},
		{"plain 400", statusError(400, []byte(`{"error":"bad request"}`), nil), false},
		{"plain 401", statusError(401, []byte(`{"error":"unauthorized"}`), nil), false},
		{"plain 404", statusError(404, []byte(`{"error":"model not found"}`), nil), false},
		{"unknown transport error", errors.New("stream stalled"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransientError(tc.err); got != tc.want {
				t.Fatalf("IsTransientError(%v) = %v want %v", tc.err, got, tc.want)
			}
		})
	}
}

// The classifiers that read the error string must keep matching a StatusError,
// since the sites it replaced built the same text with fmt.Errorf.
func TestStatusErrorStillClassifiesByBody(t *testing.T) {
	overflow := statusError(400, []byte(`{"error":{"code":"context_length_exceeded"}}`), nil)
	if !strings.Contains(overflow.Error(), "context_length_exceeded") {
		t.Fatalf("overflow body lost from error string: %v", overflow)
	}
	oom := statusError(500, []byte(oomBody), nil)
	if !IsMemoryFailure(oom) {
		t.Fatalf("IsMemoryFailure(%v) = false", oom)
	}
}

// A 200-shaped JSON body on an error status is still an error; make sure a
// streaming chat surfaces the Retry-After the same way ChatOnce does.
func TestContinuousChatSurfacesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After-Ms", "250")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "too many requests"})
	}))
	defer server.Close()
	host := OllamaHost{uri: server.URL}

	_, errCh := host.ContinuousChat(context.Background(), ChatRequest{Model: "m"})
	err := <-errCh
	if err == nil {
		t.Fatal("expected the 429 to surface")
	}
	d, ok := RetryAfterDelay(err)
	if !ok || d != 250*time.Millisecond {
		t.Fatalf("RetryAfterDelay(%v) = %v, %v want 250ms, true", err, d, ok)
	}
}
