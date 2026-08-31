package middleware

import (
	"net/url"
	"strings"
	"testing"
)

// The whole value of this field is that it is diagnosable without being
// identifying. Both halves are load-bearing and both are tested here: a
// version that leaked values would be a privacy defect (#346 is open on
// exactly that — customer emails reaching edge logs via query strings),
// and a version that redacted everything would leave us back where we
// started, unable to tell which /v1/assets request took 9.7 seconds.

func TestQueryShapeKeepsPlanSelectingValues(t *testing.T) {
	// The measured case: same route, same limit, 18x apart in latency —
	// 82 ms vs 1523 ms on r1 — distinguishable only by order_by.
	u, err := url.Parse("/v1/assets?limit=500&order_by=volume_24h_usd_desc")
	if err != nil {
		t.Fatal(err)
	}
	got := QueryShape(u)
	for _, want := range []string{"limit=500", "order_by=volume_24h_usd_desc"} {
		if !strings.Contains(got, want) {
			t.Errorf("shape %q lost %q — without it an operator cannot tell which "+
				"query plan was slow, which is the entire point of the field", got, want)
		}
	}
}

func TestQueryShapeRedactsEverythingNotAllowListed(t *testing.T) {
	// Free-text, identifying, and credential-bearing parameters must
	// never contribute a value. The list is deliberately not exhaustive:
	// the design is allow-list, so an unknown parameter is redacted by
	// construction rather than by remembering to deny it.
	u, err := url.Parse(
		"/v1/x?q=alice%40example.com&email=bob%40example.com&api_key=sk_live_abc123" +
			"&token=eyJhbGciOi&cursor=100%3Aabc&account=GABC123&limit=10")
	if err != nil {
		t.Fatal(err)
	}
	got := QueryShape(u)

	for _, leaked := range []string{
		"alice", "bob", "example.com", "sk_live_abc123", "eyJhbGciOi", "GABC123", "100:abc",
	} {
		if strings.Contains(got, leaked) {
			t.Errorf("shape %q leaked %q — query values can carry API keys and PII "+
				"(#346); only allow-listed plan-selecting parameters may show a value",
				got, leaked)
		}
	}
	// The NAMES must survive: a cursor page is a different query plan
	// from a first page, and the name alone distinguishes them.
	for _, want := range []string{"cursor=<set>", "q=<set>", "limit=10"} {
		if !strings.Contains(got, want) {
			t.Errorf("shape %q missing %q — the parameter's presence is what makes "+
				"the request shape distinct", got, want)
		}
	}
}

func TestQueryShapeIsDeterministic(t *testing.T) {
	// Operators group by this string. Map iteration order would make the
	// same request produce different shapes and defeat that.
	a, _ := url.Parse("/v1/assets?order_by=volume_24h_usd_desc&limit=100&type=classic")
	b, _ := url.Parse("/v1/assets?type=classic&limit=100&order_by=volume_24h_usd_desc")
	if QueryShape(a) != QueryShape(b) {
		t.Errorf("same request, different shapes:\n  %q\n  %q", QueryShape(a), QueryShape(b))
	}
}

func TestQueryShapeEmptyWhenNoQuery(t *testing.T) {
	// The caller omits the field entirely on "" — logging an empty
	// query_shape on every fast request would be noise.
	u, _ := url.Parse("/v1/healthz")
	if got := QueryShape(u); got != "" {
		t.Errorf("QueryShape(no query) = %q, want empty", got)
	}
	if got := QueryShape(nil); got != "" {
		t.Errorf("QueryShape(nil) = %q, want empty", got)
	}
}

func TestQueryShapeResistsLogInjectionAndFlooding(t *testing.T) {
	// An allow-listed parameter is still attacker-controlled. A newline
	// would split one log record into two — a forged entry. A megabyte
	// would be a journal-flooding channel.
	u, err := url.Parse("/v1/assets?limit=" + url.QueryEscape("50\ninjected=evil"))
	if err != nil {
		t.Fatal(err)
	}
	if got := QueryShape(u); strings.ContainsAny(got, "\n\r") {
		t.Errorf("shape %q contains a control character — a newline in a logged "+
			"value forges a second log record", got)
	}

	long, err := url.Parse("/v1/assets?limit=" + strings.Repeat("9", 5000))
	if err != nil {
		t.Fatal(err)
	}
	if got := QueryShape(long); len(got) > maxShapeValueLen+64 {
		t.Errorf("shape length %d is unbounded — an allow-listed parameter must not "+
			"become a journal-flooding channel", len(got))
	}
}

func TestQueryShapeHandlesMalformedQuery(t *testing.T) {
	// A caller sending a broken query is worth seeing, not dropping.
	// url.ParseQuery returns partial results with an error; we keep them.
	u := &url.URL{Path: "/v1/assets", RawQuery: "limit=100&%zz=broken"}
	got := QueryShape(u)
	if !strings.Contains(got, "limit=100") {
		t.Errorf("shape %q dropped the parseable part of a malformed query", got)
	}
}
