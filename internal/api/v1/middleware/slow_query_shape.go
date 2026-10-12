package middleware

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// The query string decides the plan behind a route like `/v1/assets`, but raw queries may carry
// API keys or PII, so a slow request logs only its SHAPE:
//
//   - An allow-listed parameter contributes `name=value`; these are small enumerations or
//     integers that cannot carry a secret or identify a person.
//   - Any other parameter contributes `name=<set>`; the name distinguishes the shape (a cursor
//     page is a different plan) and the value never appears.

// shapeSafeParams are query parameters whose VALUES may be logged.
//
// A parameter belongs here only if its value is a fixed enumeration or bounded number AND changes
// the query plan. Free-text, identifying and credential-bearing parameters are redacted by
// omission.
var shapeSafeParams = map[string]struct{}{
	"limit":            {},
	"order_by":         {},
	"order":            {},
	"type":             {},
	"asset_class":      {},
	"kind":             {},
	"include":          {},
	"granularity":      {},
	"interval":         {},
	"window":           {},
	"source":           {},
	"quote":            {},
	"price_type":       {},
	"resolution":       {},
	"include_unmapped": {},
}

// maxShapeValueLen bounds a logged value so an allow-listed parameter cannot flood the journal;
// legitimate values are short.
const maxShapeValueLen = 48

// maxShapeNameLen bounds a logged parameter NAME, which reaches the log verbatim (never
// allow-listed) and is otherwise as unbounded a flooding channel as a value.
const maxShapeNameLen = 48

// maxShapeParts bounds how many distinct parameters contribute a term, since the COUNT is a
// flooding channel independent of any single name or value length.
const maxShapeParts = 32

// boundedLogField strips control characters (a raw newline splits one log line into two) and caps
// length. Every caller-controlled log field in QueryShape and Logger goes through it.
func boundedLogField(s string, maxLen int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

// QueryShape renders a request's query string as a deterministic, non-identifying shape (sorted, capped by maxShapeParts), or "" when there is none.
func QueryShape(u *url.URL) string {
	if u == nil || u.RawQuery == "" {
		return ""
	}
	// Malformed query strings still deserve a shape — ParseQuery returns
	// what it could parse alongside the error, and a caller sending a
	// broken query is exactly the kind of thing worth seeing.
	values, _ := url.ParseQuery(u.RawQuery)
	if len(values) == 0 {
		return ""
	}

	parts := make([]string, 0, len(values))
	for name, vals := range values {
		if _, safe := shapeSafeParams[name]; !safe {
			parts = append(parts, boundedLogField(name, maxShapeNameLen)+"=<set>")
			continue
		}
		v := ""
		if len(vals) > 0 {
			v = vals[0]
		}
		v = boundedLogField(v, maxShapeValueLen)
		parts = append(parts, name+"="+v)
	}
	sort.Strings(parts)
	if len(parts) > maxShapeParts {
		parts = append(parts[:maxShapeParts], "…(truncated)")
	}
	return strings.Join(parts, "&")
}

// QueryShapeOf is the http.Request convenience form.
func QueryShapeOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	return QueryShape(r.URL)
}
