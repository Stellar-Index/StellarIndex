package client

import (
	"encoding/json"
	"fmt"
	"strings"
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
	// URL like `https://api.ratesengine.net/errors/missing-asset`.
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
}

// Error implements the error interface.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rates-engine API error %d", e.Status)
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
// doesn't have the expected shape.
func parseAPIError(status int, contentType string, body []byte) *APIError {
	apiErr := &APIError{Status: status}

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
