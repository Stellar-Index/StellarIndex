package middleware

import (
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Diagnosing a slow endpoint means knowing WHICH request was slow, and
// on this API that is decided almost entirely by the query string.
// `/v1/assets` is one route and many queries: the limit picks a cache
// key, `order_by` picks a different index path, a cursor turns a first
// page into a keyset scan. The access log records `path` only, so a
// slow-request investigation could see "/v1/assets was 9.7s" and not
// which of those it was.
//
// The obvious fix is wrong. The Logger doc says plainly that it does
// not log query parameters because they "may carry API keys or PII",
// and that is not hypothetical here — issue #346 is open on customer
// emails reaching edge logs through query strings. Logging the raw
// query to diagnose latency would create a privacy defect to fix a
// performance one.
//
// So this records the SHAPE of a request, never its content:
//
//   - A parameter on the allow-list below contributes `name=value`.
//     These are the ones that select a query plan — limits, orderings,
//     granularities, type filters. Their values are drawn from small
//     enumerations or are integers; none can carry a secret or identify
//     a person.
//   - Any other parameter contributes `name=<set>`. The NAME is what
//     makes a request shape distinct (a cursor page is a different plan
//     from a first page), and the name alone is enough to tell those
//     apart. The value never appears.
//
// The result is that an operator sees `limit=500&order_by=volume_24h_usd_desc`
// or `cursor=<set>&q=<set>`, which is exactly enough to reproduce the
// slow request, and never enough to leak one.

// shapeSafeParams are query parameters whose VALUES may be logged.
//
// The bar for adding one: the value must come from a fixed enumeration
// or be a bounded number, AND it must change which query plan runs.
// Anything free-text (`q`), anything identifying (`email`, `account`),
// and anything credential-bearing (`api_key`, `token`) is excluded by
// construction — it is not on this list, so it is redacted.
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

// maxShapeValueLen bounds a logged value so a hostile caller cannot use
// an allow-listed parameter as a journal-flooding channel. The real
// values are short (`500`, `volume_24h_usd_desc`); anything longer is
// not a legitimate use of these parameters.
const maxShapeValueLen = 48

// QueryShape renders a request's query string as a diagnosable,
// non-identifying shape. Returns "" when there is no query string, so
// the caller can omit the field entirely rather than log an empty one.
//
// Deterministic: parameters are sorted, so the same shape produces the
// same string and an operator can group by it.
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
			parts = append(parts, name+"=<set>")
			continue
		}
		v := ""
		if len(vals) > 0 {
			v = vals[0]
		}
		if len(v) > maxShapeValueLen {
			v = v[:maxShapeValueLen] + "…"
		}
		// A safe-listed parameter carrying a newline would break the log
		// line into two records — a log-injection primitive. Strip the
		// control characters rather than trusting the enumeration.
		v = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, v)
		parts = append(parts, name+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

// QueryShapeOf is the http.Request convenience form.
func QueryShapeOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	return QueryShape(r.URL)
}
