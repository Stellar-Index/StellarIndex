package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is the typed wrapper around an RFC 9457 problem+json
// response. Returned from any client method when the server replies
// with HTTP 4xx or 5xx. Clients detect and inspect via [errors.As]:
//
//	var apiErr *client.APIError
//	if errors.As(err, &apiErr) {
//	    switch apiErr.Status {
//	    case 404: ...
//	    case 429: ...
//	    case 503: ...
//	    }
//	}
type APIError struct {
	// Status is the HTTP status code (4xx / 5xx).
	Status int

	// Type is the problem+json `type` field — a stable error-type
	// URL like `https://api.stellarindex.io/errors/missing-asset`.
	// Empty when the server returned a non-problem+json body
	// (e.g. plain-text 502 from a reverse proxy).
	Type string

	// Title is a short human-readable headline ("Asset not found").
	Title string

	// Detail is the freeform per-request message ("USDC-G... has
	// no observations"). May be empty.
	Detail string

	// Instance typically echoes the request URL.
	Instance string

	// RequestID echoes the X-Request-ID extension field. Useful
	// for correlating with server logs in support tickets.
	RequestID string

	// RetryAfter is the parsed value of the HTTP `Retry-After`
	// response header, when present (G22-08). The server sets it on
	// 429 (rate-limited) and 503 (service-unavailable) responses to
	// tell the caller how long to back off. Zero when the header was
	// absent or unparseable. Both wire forms are supported: a
	// delta-seconds integer (RFC 9110 §10.2.3) is taken as a relative
	// duration; an HTTP-date is converted to the remaining duration
	// from now (never negative). Callers typically sleep for this
	// value before retrying a 429 / 503.
	RetryAfter time.Duration
}

// RetryAfterDuration reports the recommended back-off and whether the
// server supplied one. ok=false means the header was absent (callers
// should fall back to their own back-off policy rather than retrying
// immediately).
func (e *APIError) RetryAfterDuration() (d time.Duration, ok bool) {
	return e.RetryAfter, e.RetryAfter > 0
}

// Error implements the error interface.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "stellar-index API error %d", e.Status)
	if e.Title != "" {
		fmt.Fprintf(&b, ": %s", e.Title)
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, " — %s", e.Detail)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request_id=%s)", e.RequestID)
	}
	return b.String()
}

// IsNotFound reports whether the error is a 404.
func (e *APIError) IsNotFound() bool { return e.Status == 404 }

// IsUnauthorized reports whether the error is a 401.
func (e *APIError) IsUnauthorized() bool { return e.Status == 401 }

// IsForbidden reports whether the error is a 403.
func (e *APIError) IsForbidden() bool { return e.Status == 403 }

// IsRateLimited reports whether the error is a 429.
func (e *APIError) IsRateLimited() bool { return e.Status == 429 }

// IsServerError reports whether the error is a 5xx.
func (e *APIError) IsServerError() bool { return e.Status >= 500 }

// parseAPIError decodes a problem+json body into an [APIError].
// Falls back to a status-only error when the body isn't JSON or
// doesn't have the expected shape. `retryAfter` is the raw value of
// the HTTP `Retry-After` response header (empty when absent).
func parseAPIError(status int, contentType, retryAfter string, body []byte) *APIError {
	apiErr := &APIError{Status: status, RetryAfter: parseRetryAfter(retryAfter)}

	// Best-effort: only try to decode JSON when the content-type
	// claims problem+json or application/json. Other content types
	// (text/plain, text/html from a misconfigured reverse proxy)
	// just produce a status-only APIError.
	if !(strings.Contains(contentType, "json")) {
		if len(body) > 0 && len(body) <= 256 {
			apiErr.Detail = strings.TrimSpace(string(body))
		}
		return apiErr
	}

	var p problemJSON
	if err := json.Unmarshal(body, &p); err != nil {
		// Malformed JSON; surface a reasonable Detail.
		apiErr.Detail = errEmptyJSON.Error()
		return apiErr
	}
	apiErr.Type = p.Type
	apiErr.Title = p.Title
	apiErr.Detail = p.Detail
	apiErr.Instance = p.Instance
	apiErr.RequestID = p.RequestID
	return apiErr
}

// parseRetryAfter converts an HTTP `Retry-After` header value into a
// back-off duration. Per RFC 9110 §10.2.3 the value is either a
// non-negative delta-seconds integer or an HTTP-date. Returns 0 for an
// empty/unparseable header, or for an HTTP-date already in the past
// (so callers never sleep on a negative duration).
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// problemJSON is the wire shape of an RFC 9457 problem+json
// response. Mirrors internal/api/v1.Problem.
type problemJSON struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}
