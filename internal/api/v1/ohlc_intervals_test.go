// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── The /v1/ohlc interval ladder has one declaration ────────────────
//
// [timescale.OHLCRoutes] is it. The parser and its 400 body derive from
// the table; the spec's enum and this package's duration() switch are
// the two copies that cannot be derived (one is YAML, the other a
// named-width lookup), so they are pinned here instead — launch plan
// W8-20's "one declaration of the served set", extended past the
// HistoryGranularity type to every interval /v1/ohlc advertises.

// TestOHLCIntervals_SpecEnumIsTheRouteTable — the published enum for
// `/v1/ohlc`'s `interval` parameter must equal the route table, in
// order. A value in one and not the other is either a documented
// interval that 400s or a served interval nobody can discover.
func TestOHLCIntervals_SpecEnumIsTheRouteTable(t *testing.T) {
	spec := loadOpenAPISpec(t)
	// Paths are keyed relative to the `/v1` server base.
	op := spec.Paths["/ohlc"]["get"]
	if op == nil {
		t.Fatal("spec has no GET /ohlc")
	}
	var enum []string
	for _, p := range op.Parameters {
		if p.Name == "interval" && p.In == "query" && p.Schema != nil {
			enum = p.Schema.Enum
		}
	}
	if len(enum) == 0 {
		t.Fatal("GET /ohlc has no inline `interval` query parameter with a schema enum")
	}
	want := make([]string, len(timescale.OHLCRoutes))
	for i, r := range timescale.OHLCRoutes {
		want[i] = r.Interval
	}
	if strings.Join(enum, ",") != strings.Join(want, ",") {
		t.Errorf("openapi /v1/ohlc interval enum = %v\n  timescale.OHLCRoutes    = %v\n"+
			"edit both (and regenerate docs/reference/api, examples/postman and "+
			"web/explorer/src/api/types.ts from the spec)", enum, want)
	}
}

// TestOHLCIntervals_EveryRouteHasADuration — default-window sizing
// multiplies limit by duration(); a route the switch does not know
// sizes a zero window, which parseOHLCSeriesFromTo then rejects as
// `to <= from` on a perfectly valid interval.
func TestOHLCIntervals_EveryRouteHasADuration(t *testing.T) {
	for _, r := range timescale.OHLCRoutes {
		if d := ohlcInterval(r.Interval).duration(); d <= 0 {
			t.Errorf("%s: duration() = %v; add the interval to the switch", r.Interval, d)
		}
	}
	if d := ohlcInterval("7h").duration(); d != 0 {
		t.Errorf("duration() of an undeclared interval = %v, want 0", d)
	}
}

// TestOHLCIntervals_ParserAcceptsExactlyTheRouteTable — parseOHLCInterval
// accepts every declared interval, refuses everything else with the
// canonical problem, and the problem's enumeration is generated from
// the same table rather than written out.
func TestOHLCIntervals_ParserAcceptsExactlyTheRouteTable(t *testing.T) {
	// The parser takes the raw value as an argument; the request only
	// feeds the problem body's `instance`, so a fixed target serves
	// every probe (including the ones no URL could carry verbatim).
	const target = "/v1/ohlc"
	for _, r := range timescale.OHLCRoutes {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		got, ok := parseOHLCInterval(rec, req, r.Interval)
		if !ok || string(got) != r.Interval {
			t.Errorf("parseOHLCInterval(%q) = %q, %v; want accepted verbatim (body: %s)",
				r.Interval, got, ok, rec.Body.String())
		}
	}
	for _, raw := range []string{"7h", "2M", "", " 1h", "1H"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if _, ok := parseOHLCInterval(rec, req, raw); ok {
			t.Errorf("parseOHLCInterval(%q) accepted; nothing declares it", raw)
			continue
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("parseOHLCInterval(%q): status %d, want 400", raw, rec.Code)
		}
		if body := rec.Body.String(); !strings.Contains(body, timescale.OHLCIntervalList()) {
			t.Errorf("parseOHLCInterval(%q): 400 body does not carry the route table's "+
				"enumeration %q:\n%s", raw, timescale.OHLCIntervalList(), body)
		}
	}
}
