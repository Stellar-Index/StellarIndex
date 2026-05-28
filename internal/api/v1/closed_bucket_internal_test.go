package v1

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestParseFromToClamped_ImplicitToSnapsToBoundary covers the
// ADR-0015 contract: when the client doesn't supply `to`, the
// handler-default (now) is rounded down to the closed-bucket
// boundary so two requests landing in the same window get the
// same answer regardless of which region serves them.
func TestParseFromToClamped_ImplicitToSnapsToBoundary(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/vwap?base=native&quote=fiat:USD", nil)
	rec := httptest.NewRecorder()

	from, to, clamped, ok := parseFromToClamped(rec, req)
	if !ok {
		t.Fatalf("parse failed: %s", rec.Body.String())
	}
	if !clamped {
		t.Errorf("clamped flag = false, want true (no `to` query param means clamp)")
	}

	// `to` MUST land on a 30 s boundary.
	if to.UnixNano()%closedBucketWindow.Nanoseconds() != 0 {
		t.Errorf("to = %s, not aligned to %s boundary",
			to.Format(time.RFC3339Nano), closedBucketWindow)
	}
	// `to` MUST be ≤ now (we clamp DOWN, not up — never serve future
	// data).
	if to.After(time.Now().UTC()) {
		t.Errorf("to = %s, after wall-clock now (clamped to a future boundary)",
			to.Format(time.RFC3339Nano))
	}
	// The implicit window must remain 1 h (matching parseFromTo's
	// default) — clamp shouldn't shrink the range.
	if got := to.Sub(from); got != time.Hour {
		t.Errorf("to-from = %s, want 1h (default range preserved across clamp)", got)
	}
}

// TestParseFromToClamped_ExplicitToPreserved covers the second half
// of the contract: when the client DID supply `to`, we honour it
// verbatim. The clamp is for default-now only — surprising clients
// who pass an exact timestamp by snapping their range would be a
// usability regression.
func TestParseFromToClamped_ExplicitToPreserved(t *testing.T) {
	// 12:34:56.500 — deliberately NOT on a 30 s boundary.
	explicit := "2026-04-27T12:34:56.500Z"
	req := httptest.NewRequest(http.MethodGet,
		"/v1/vwap?base=native&quote=fiat:USD&to="+explicit, nil)
	rec := httptest.NewRecorder()

	_, to, clamped, ok := parseFromToClamped(rec, req)
	if !ok {
		t.Fatalf("parse failed: %s", rec.Body.String())
	}
	if clamped {
		t.Errorf("clamped flag = true, want false (explicit `to` should not clamp)")
	}
	want, _ := time.Parse(time.RFC3339Nano, explicit)
	if !to.Equal(want) {
		t.Errorf("to = %s, want %s (verbatim)",
			to.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// TestParseFromToClamped_TwoCallsSameWindowAgree is the cross-region
// consistency property in test form. Two requests that land at
// different sub-second moments within the same 30 s window must
// resolve to the SAME (from, to) pair. This is what makes
// "all 3 regions return the same rate" true rather than aspirational.
func TestParseFromToClamped_TwoCallsSameWindowAgree(t *testing.T) {
	// Skip on the unlucky case where the test crosses a boundary
	// mid-execution. Highly unlikely in practice (test runs in
	// microseconds), but deterministic.
	first := time.Now().UTC().Truncate(closedBucketWindow)

	req1 := httptest.NewRequest(http.MethodGet, "/v1/vwap?base=native&quote=fiat:USD", nil)
	from1, to1, _, ok1 := parseFromToClamped(httptest.NewRecorder(), req1)
	if !ok1 {
		t.Fatalf("first call failed")
	}

	// Second call landing within the same window — by truncating to
	// the same boundary, both calls MUST resolve to identical times.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/vwap?base=native&quote=fiat:USD", nil)
	from2, to2, _, ok2 := parseFromToClamped(httptest.NewRecorder(), req2)
	if !ok2 {
		t.Fatalf("second call failed")
	}

	// If we crossed a boundary mid-test, the test isn't telling us
	// anything useful — re-anchor and bail with a notice.
	if to1.Before(first) {
		t.Skipf("test straddled a %s boundary; re-run", closedBucketWindow)
	}

	if !from1.Equal(from2) || !to1.Equal(to2) {
		t.Errorf("two same-window calls disagreed:\n"+
			"  call 1 = [%s, %s)\n"+
			"  call 2 = [%s, %s)",
			from1.Format(time.RFC3339Nano), to1.Format(time.RFC3339Nano),
			from2.Format(time.RFC3339Nano), to2.Format(time.RFC3339Nano))
	}
}

// TestParseFromToClamped_ExplicitFromNotShifted covers the edge case
// where the client pinned `from` but defaulted `to`. The clamp moves
// `to` to a boundary; `from` MUST stay where the client put it.
func TestParseFromToClamped_ExplicitFromNotShifted(t *testing.T) {
	explicitFrom := "2026-04-27T10:00:00Z"
	req := httptest.NewRequest(http.MethodGet,
		"/v1/vwap?base=native&quote=fiat:USD&from="+explicitFrom, nil)
	rec := httptest.NewRecorder()

	from, _, _, ok := parseFromToClamped(rec, req)
	if !ok {
		t.Fatalf("parse failed: %s", rec.Body.String())
	}
	want, _ := time.Parse(time.RFC3339, explicitFrom)
	if !from.Equal(want) {
		t.Errorf("from = %s, want %s (explicit from should not be shifted)",
			from.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

// TestParseFromTo_WindowParam covers F-0072 closure: the `window=`
// shortcut sets `from = to - duration` so CG-style customers don't
// have to compute it. Pre-F-0072 the param was silently ignored,
// users got the 1h default, and a 24h-window request returned a
// confusing 404 over the wrong range.
func TestParseFromTo_WindowParam(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		wantDur time.Duration
	}{
		{"hours", "?window=24h", 24 * time.Hour},
		{"minutes", "?window=30m", 30 * time.Minute},
		{"days shortcut", "?window=7d", 7 * 24 * time.Hour},
		{"compound", "?window=1h30m", 90 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/twap"+tc.query, nil)
			rec := httptest.NewRecorder()
			from, to, ok := parseFromTo(rec, req)
			if !ok {
				t.Fatalf("parse failed: %s", rec.Body.String())
			}
			got := to.Sub(from)
			if got != tc.wantDur {
				t.Errorf("window=%s → to-from = %s, want %s",
					tc.query, got, tc.wantDur)
			}
		})
	}
}

// TestParseFromTo_WindowAndFrom_Conflict pins F-0072's loud-rejection
// rule. Passing both `window` and `from` is ambiguous (they both
// control the start time); a 400 is louder than picking one
// silently — which is what pre-F-0072 did when `window` was ignored.
func TestParseFromTo_WindowAndFrom_Conflict(t *testing.T) {
	url := "/v1/twap?window=24h&from=2026-01-01T00:00:00Z"
	req := httptest.NewRequest(http.MethodGet, url, nil)
	rec := httptest.NewRecorder()
	if _, _, ok := parseFromTo(rec, req); ok {
		t.Fatal("parse succeeded; want 400 mutually-exclusive")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestParseFromTo_InvalidWindow rejects malformed `window` values.
func TestParseFromTo_InvalidWindow(t *testing.T) {
	cases := []string{"garbage", "1x", "1d2h", "-5h", "0"}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/twap?window="+q, nil)
			rec := httptest.NewRecorder()
			if _, _, ok := parseFromTo(rec, req); ok {
				t.Fatalf("parse succeeded on %q; want 400", q)
			}
			if rec.Code != http.StatusBadRequest {
				t.Errorf("window=%q status = %d, want 400", q, rec.Code)
			}
		})
	}
}
