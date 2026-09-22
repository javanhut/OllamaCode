package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// StatusError is an HTTP error response from a provider. It keeps the status
// code and body text separately (the error STRING embeds both, exactly as the
// fmt.Errorf sites it replaced did, so the body-pattern classifiers —
// IsContextOverflow, IsFormatRejection, IsMemoryFailure — keep matching), and
// carries the provider's Retry-After hint for the stream retry backoff.
type StatusError struct {
	Code int
	Body string

	retryAfter    time.Duration
	hasRetryAfter bool
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("unexpected status code: %d", e.Code)
	}
	return fmt.Sprintf("unexpected status code: %d: %s", e.Code, e.Body)
}

// RetryAfter returns the delay the provider asked for via Retry-After /
// Retry-After-Ms, and whether it sent a parseable one.
func (e *StatusError) RetryAfter() (time.Duration, bool) {
	return e.retryAfter, e.hasRetryAfter
}

// RetryAfterDelay extracts the provider-requested retry delay from any error
// carrying a *StatusError. Callers should honor it over their own backoff —
// the provider explicitly asked — but still cap it at something sane.
func RetryAfterDelay(err error) (time.Duration, bool) {
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.RetryAfter()
	}
	return 0, false
}

// StatusCodeOf returns the HTTP status code of an error built by statusError,
// falling back to the "unexpected status code: N" string form so errors from
// paths that still use fmt.Errorf classify the same.
func StatusCodeOf(err error) int {
	if err == nil {
		return 0
	}
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Code
	}
	var code int
	if _, err := fmt.Sscanf(err.Error(), "unexpected status code: %d", &code); err == nil {
		return code
	}
	return 0
}

// parseRetryAfter reads the Retry-After / Retry-After-Ms response headers.
// Retry-After may be delay-seconds or an HTTP-date (RFC 9110 §10.2.3);
// Retry-After-Ms is the millisecond variant some providers send alongside it.
func parseRetryAfter(header http.Header) (time.Duration, bool) {
	if header == nil {
		return 0, false
	}
	if ms := strings.TrimSpace(header.Get("Retry-After-Ms")); ms != "" {
		if v, err := strconv.ParseFloat(ms, 64); err == nil && v > 0 {
			return time.Duration(v * float64(time.Millisecond)), true
		}
	}
	if s := strings.TrimSpace(header.Get("Retry-After")); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 {
			return time.Duration(v) * time.Second, true
		}
		if t, err := http.ParseTime(s); err == nil {
			if d := time.Until(t); d > 0 {
				return d, true
			}
			// A date already past means "retry now" — a valid hint, zero delay.
			return 0, true
		}
	}
	return 0, false
}

// transientMarkers are the phrases a provider puts in the BODY of a retryable
// refusal. The status code alone cannot classify these — a shared gateway may
// answer overload with an ambiguous 4xx — so the match is on the text, the
// same convention the overflow and memory classifiers use.
var transientMarkers = []string{
	"overloaded",
	"rate limit",
	"rate_limit",
	"too many requests",
	"connection reset",
	"connection refused",
	"eof",
	"timeout",
	"timed out",
	"temporarily unavailable",
	"service unavailable",
	"bad gateway",
	"gateway timeout",
}

// IsTransientError reports whether err looks like a failure that a retry could
// win: a transient status code (408, 429, 5xx) or any status whose body reads
// as overload, rate limiting, or a broken connection. Permanent refusals —
// 400/401/403/404 without transient text, context overflow, format rejection,
// OOM — are classified elsewhere and stay non-retryable.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range transientMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	switch code := StatusCodeOf(err); {
	case code == http.StatusRequestTimeout,
		code == http.StatusTooManyRequests,
		code >= 500:
		return true
	}
	return false
}
