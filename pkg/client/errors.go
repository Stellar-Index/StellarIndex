package client

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// APIError is the typed wrapper around an RFC 9457 problem+json response, returned from any client method on HTTP 4xx or 5xx; inspect it with [errors.As].
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

	// RetryAfter is the parsed HTTP `Retry-After` response header, set by the server on 429
	// (rate-limited) and 503 (service-unavailable) to say how long to back off. Zero when absent
	// or unparseable. A delta-seconds integer (RFC 9110 §10.2.3) is taken as a relative duration;
	// an HTTP-date is converted to the remaining duration from now (never negative). Callers
	// typically sleep for this value before retrying.
	RetryAfter time.Duration

	// CoverageFrom is the `coverage_from` extension member on /v1/price/at's 404: the earliest instant
	// the server holds price history for the requested pair (the same field the 2xx envelope carries,
	// [Envelope.CoverageFrom]). Nil means the server did not say, never "from the beginning of time".
	CoverageFrom *time.Time

	// OutsideCoverage is the `outside_coverage` extension member on the same 404: true when the
	// requested instant is at or before CoverageFrom, i.e. before the server's history for this pair
	// rather than a gap inside it. Only true when CoverageFrom is also set; false carries no
	// information (the server omits it when the floor is unknown).
	OutsideCoverage bool

	// Substance is the `substance` extension member on a thin-market
	// `price-withheld` 404: the measurement the price was withheld on.
	Substance *SubstanceEvidence

	// Reason is the `reason` extension member on a `price-withheld` 404:
	// the machine-readable cause (e.g. "substance", "scam_issuer"). Branch
	// on this rather than on Title. Empty on every other error.
	Reason string

	// MonthlyQuota is the `monthly_quota` extension member on a
	// MonthlyQuota-shaped 429 (middleware.MonthlyQuota's
	// monthly-quota-exceeded and monthly-quota-unavailable bodies): the
	// account's monthly request cap. Nil on any other error, including
	// a plain rate-limit 429.
	MonthlyQuota *int64

	// MonthToDate is the `month_to_date` extension member on a
	// monthly-quota-exceeded 429 only: the account's month-to-date
	// request count that triggered the cap. Nil on
	// monthly-quota-unavailable (the counter read failed, so the server
	// has no honest value to report) and on any other error.
	MonthToDate *int64
}

// RetryAfterDuration reports the server's recommended back-off; ok=false means no header was sent, so fall back to your own back-off rather than retrying immediately.
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

// Problem `type` URLs a caller branches on. A 404 on a price surface is
// one of two different answers, and only Type tells them apart.
const (
	// ProblemTypePriceNotFound: the server holds no price data for the pair.
	ProblemTypePriceNotFound = "https://api.stellarindex.io/errors/price-not-found"
	// ProblemTypePriceWithheld: the server observes the pair but a serving
	// gate declined to publish a price. Never render this as "no data".
	ProblemTypePriceWithheld = "https://api.stellarindex.io/errors/price-withheld"
)

// IsNotFound reports whether the error is a 404 of ANY kind, including a withheld price, so check [APIError.IsWithheld] first.
func (e *APIError) IsNotFound() bool { return e.Status == 404 }

// IsWithheld reports whether the server declined to publish a price it holds (problem type [ProblemTypePriceWithheld]).
func (e *APIError) IsWithheld() bool { return e.Type == ProblemTypePriceWithheld }

// IsUnauthorized reports whether the error is a 401.
func (e *APIError) IsUnauthorized() bool { return e.Status == 401 }

// IsForbidden reports whether the error is a 403.
func (e *APIError) IsForbidden() bool { return e.Status == 403 }

// IsRateLimited reports whether the error is a 429.
func (e *APIError) IsRateLimited() bool { return e.Status == 429 }

// IsServerError reports whether the error is a 5xx.
func (e *APIError) IsServerError() bool { return e.Status >= 500 }

// parseAPIError decodes a problem+json body into an [APIError], falling back to a status-only error when the body is not JSON or
// lacks the expected shape. retryAfter is the raw Retry-After header (empty when absent).
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
	apiErr.CoverageFrom = p.CoverageFrom
	apiErr.OutsideCoverage = p.OutsideCoverage
	apiErr.Substance = p.Substance
	apiErr.Reason = p.Reason
	apiErr.MonthlyQuota = p.MonthlyQuota
	apiErr.MonthToDate = p.MonthToDate

	// Valid JSON that carried NONE of the problem+json fields (e.g. a
	// proxy or non-conforming service replying
	// {"error":"..","trace_id":".."}). Don't silently discard it:
	// fall back to the same body-preserving behaviour the non-JSON
	// branch uses so the caller still sees the message/id.
	if p.Type == "" && p.Title == "" && p.Detail == "" && p.Instance == "" && p.RequestID == "" {
		if len(body) > 0 && len(body) <= 256 {
			apiErr.Detail = strings.TrimSpace(string(body))
		}
	}
	return apiErr
}

// parseRetryAfter converts a Retry-After header (RFC 9110 §10.2.3: delta-seconds or HTTP-date) into a back-off duration. Returns 0
// for empty, unparseable, or past-dated values so callers never sleep on a negative duration.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		// Overflow guard: time.Duration is int64 nanoseconds, so a value above ~292 years wraps negative
		// and a caller sleeping on it would retry immediately. Return 0 (the absent/unparseable sentinel)
		// rather than clamp: RetryAfter reports what the wire said; a ceiling is the caller's policy.
		// Compared in int64 because an `int` constant conversion overflows on 32-bit.
		if int64(secs) > math.MaxInt64/int64(time.Second) {
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

// problemJSON is the wire shape of an RFC 9457 problem+json response. It mirrors internal/api/v1.Problem and the spec's Problem
// schema; TestProblemJSONMatchesSpec holds them in lockstep.
type problemJSON struct {
	Type            string     `json:"type"`
	Title           string     `json:"title"`
	Status          int        `json:"status"`
	Detail          string     `json:"detail,omitempty"`
	Instance        string     `json:"instance,omitempty"`
	RequestID       string     `json:"request_id,omitempty"`
	CoverageFrom    *time.Time `json:"coverage_from,omitempty"`
	OutsideCoverage bool       `json:"outside_coverage,omitempty"`
	MonthlyQuota    *int64     `json:"monthly_quota,omitempty"`
	MonthToDate     *int64     `json:"month_to_date,omitempty"`

	Substance *SubstanceEvidence `json:"substance,omitempty"`
	Reason    string             `json:"reason,omitempty"`
}
