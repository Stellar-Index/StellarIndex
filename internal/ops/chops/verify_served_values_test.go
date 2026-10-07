// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunServedValueChecks_TolerancesAndOutages exercises the three
// outcome classes against stub servers: within tolerance (ok),
// drifted beyond tolerance (fail — the CS-010 class), and
// ground-truth outage (SKIPPED, NaN rel_err — a dark truth source
// must not read as a served-value failure, AND must not read as a
// pass either: skipped never sets ok, closing the F5 fail-open where
// a dark truth source would mask a real drift behind ok=1).
func TestRunServedValueChecks_TolerancesAndOutages(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name        string
		served      float64
		truth       float64
		tolerance   float64
		truthFails  bool
		wantOK      bool
		wantSkipped bool
		wantNaN     bool
	}{
		{"within tolerance", 105_000_000_000, 105_400_000_000, 0.005, false, true, false, false},
		{"cs-010 class drift fails", 105_000_000_000, 66_000_000_000, 0.02, false, false, false, false},
		{"truth outage skips", 105_000_000_000, 0, 0.005, true, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{"total_supply":"105000000000"}}`))
			}))
			t.Cleanup(api.Close)
			truth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.truthFails {
					http.Error(w, "down", http.StatusBadGateway)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"totalSupply":"66000000000"}`))
			}))
			t.Cleanup(truth.Close)

			check := servedValueCheck{
				name: "probe", tolerance: tc.tolerance,
				// decimals=0: the stub serves natural units; the
				// base-unit scaling itself is pinned by
				// TestServedSupplyField_ScalesBaseUnits.
				served: servedSupplyField("native", "total_supply", 0),
				truth: func(ctx context.Context, c *http.Client) (*big.Rat, error) {
					var body map[string]any
					if err := getJSON(ctx, c, truth.URL, &body); err != nil {
						return nil, err
					}
					return new(big.Rat).SetFloat64(tc.truth), nil
				},
			}
			results := runChecksForTest(ctx, api.URL, []servedValueCheck{check})
			r := results[0]
			if r.ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (rel_err=%v note=%s)", r.ok, tc.wantOK, r.relErr, r.note)
			}
			if r.skipped != tc.wantSkipped {
				t.Errorf("skipped = %v, want %v (note=%s)", r.skipped, tc.wantSkipped, r.note)
			}
			if tc.wantNaN != math.IsNaN(r.relErr) {
				t.Errorf("NaN rel_err = %v, want %v", math.IsNaN(r.relErr), tc.wantNaN)
			}
		})
	}
}

// TestReconcileSkippedNeverAssertsOK is the direct F5 regression pin:
// a skipped (truth-dark) check must render served_value_skipped=1 and
// NO served_value_ok line at all — never ok=1, which would hide a real
// drift behind an unavailable ground truth.
func TestReconcileSkippedNeverAssertsOK(t *testing.T) {
	body := renderServedValueProm([]servedValueResult{
		{name: "dark", relErr: math.NaN(), skipped: true},
	}, nil, time.Unix(1_751_000_000, 0))
	if !strings.Contains(body, `stellarindex_served_value_skipped{check="dark"} 1`) {
		t.Errorf("skipped check must emit served_value_skipped=1:\n%s", body)
	}
	if strings.Contains(body, `stellarindex_served_value_ok{check="dark"}`) {
		t.Errorf("skipped check must NOT emit any served_value_ok line (F5 fail-open):\n%s", body)
	}
}

// TestServedFetchErrorIsNotAZeroRelErr — our own surface failing to answer
// is a failed check, not a measured perfect match: it must render ok=0 and
// no rel_err sample, never rel_err 0.
func TestServedFetchErrorIsNotAZeroRelErr(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(api.Close)
	check := servedValueCheck{
		name: "probe", tolerance: 0.005,
		served: servedSupplyField("native", "total_supply", 0),
		truth:  func(context.Context, *http.Client) (*big.Rat, error) { return big.NewRat(100, 1), nil },
	}
	results := runChecksForTest(context.Background(), api.URL, []servedValueCheck{check})
	if results[0].ok || results[0].skipped {
		t.Fatalf("served fetch error must be a failed check, got ok=%v skipped=%v", results[0].ok, results[0].skipped)
	}
	body := renderServedValueProm(results, nil, time.Unix(1_751_000_000, 0))
	if strings.Contains(body, `stellarindex_served_value_rel_err{check="probe"}`) {
		t.Errorf("served fetch error must not emit a rel_err sample:\n%s", body)
	}
	if !strings.Contains(body, `stellarindex_served_value_ok{check="probe"} 0`) {
		t.Errorf("served fetch error must emit ok=0:\n%s", body)
	}
}

// TestServedValueLastRunOnlyWhenAVerdictWasReached —served_value_check_stale keys on
// last_run_unix, so an all-skipped run (every truth source dark, nothing
// verified) must not refresh it; a run with any verdict must.
func TestServedValueLastRunOnlyWhenAVerdictWasReached(t *testing.T) {
	const lastRun = "stellarindex_served_value_last_run_unix 1751000000"
	now := time.Unix(1_751_000_000, 0)
	dark := servedValueResult{name: "dark", relErr: math.NaN(), skipped: true}
	darkList := &reserveListResult{skipped: true}

	if body := renderServedValueProm([]servedValueResult{dark}, darkList, now); strings.Contains(body, lastRun) {
		t.Errorf("all-skipped run must not refresh last_run_unix:\n%s", body)
	}
	if body := renderServedValueProm([]servedValueResult{dark, {name: "a", relErr: 0.5}}, nil, now); !strings.Contains(body, lastRun) {
		t.Errorf("a run with a failed verdict must refresh last_run_unix:\n%s", body)
	}
	if body := renderServedValueProm([]servedValueResult{dark}, &reserveListResult{}, now); !strings.Contains(body, lastRun) {
		t.Errorf("a verified reserve-list check must refresh last_run_unix:\n%s", body)
	}
}

// runChecksForTest runs the production runner over an injected check table.
func runChecksForTest(ctx context.Context, apiBase string, checks []servedValueCheck) []servedValueResult {
	return runServedValueChecks(ctx, &http.Client{Timeout: 5 * time.Second}, apiBase, checks, 5*time.Second, io.Discard)
}

// TestRenderServedValueProm — the textfile body has the three gauge
// families, quotes check names, and omits rel_err for NaN.
func TestRenderServedValueProm(t *testing.T) {
	body := renderServedValueProm([]servedValueResult{
		{name: "a", relErr: 0.001, ok: true},
		{name: "b", relErr: math.NaN(), ok: true},
	}, nil, time.Unix(1_751_000_000, 0))
	for _, want := range []string{
		`stellarindex_served_value_rel_err{check="a"} 0.001`,
		`stellarindex_served_value_ok{check="a"} 1`,
		`stellarindex_served_value_ok{check="b"} 1`,
		`stellarindex_served_value_skipped{check="a"} 0`,
		`stellarindex_served_value_skipped{check="b"} 0`,
		"stellarindex_served_value_last_run_unix 1751000000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("textfile body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `rel_err{check="b"}`) {
		t.Error("NaN rel_err must be omitted, not rendered")
	}
}

// TestServedSupplyField_NullIsAFailure — a null served supply field
// is a pipeline failure, not a zero.
func TestServedSupplyField_NullIsAFailure(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"total_supply":null}}`))
	}))
	t.Cleanup(api.Close)
	_, err := servedSupplyField("native", "total_supply", 7)(context.Background(), &http.Client{}, api.URL)
	if err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("want null-field error, got %v", err)
	}
}

// TestServedSupplyField_ScalesBaseUnits pins the empirical 2026-07-02
// finding: the F2 supply fields are BASE-UNIT decimal strings
// (stroops for classic), so the reader must scale by 10^-decimals
// before comparing against natural-unit ground truth.
func TestServedSupplyField_ScalesBaseUnits(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"total_supply":"500018068120000000"}}`)) // stroops
	}))
	t.Cleanup(api.Close)
	got, err := servedSupplyField("native", "total_supply", 7)(context.Background(), &http.Client{}, api.URL)
	if err != nil {
		t.Fatal(err)
	}
	if want := big.NewRat(50_001_806_812, 1); got.value.Cmp(want) != 0 {
		t.Fatalf("scaled supply = %s, want %s", got.value.RatString(), want.RatString())
	}
}

// TestServedValuesExitError pins the F5 all-skip fail-closed refinement
// (reviewer #3): a run where EVERY check skipped verified nothing and must not
// exit clean, while a partial skip stays clean and any drift fails.
func TestServedValuesExitError(t *testing.T) {
	cases := []struct {
		name                   string
		total, failed, skipped int
		wantErr                bool
	}{
		{"all verified clean", 3, 0, 0, false},
		{"a drift fails", 3, 1, 0, true},
		{"partial skip stays clean (some verified)", 3, 0, 1, false},
		{"partial skip with the rest verified", 3, 0, 2, false},
		{"ALL skipped fails closed — verified nothing", 3, 0, 3, true},
		{"single check all-skipped fails closed", 1, 0, 1, true},
		{"empty run (no checks) is not an all-skip failure", 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := servedValuesExitError(tc.total, tc.failed, tc.skipped)
			if tc.wantErr != (err != nil) {
				t.Fatalf("servedValuesExitError(total=%d, failed=%d, skipped=%d) err=%v, wantErr=%v",
					tc.total, tc.failed, tc.skipped, err, tc.wantErr)
			}
		})
	}
}

// TestServedValuesVerdict_ListCheckIsNotAValueCheck — the reserve-list
// check verifies no served NUMBER, so it must not count toward the value
// checks' all-skipped guard: every value source dark with a verified list
// still verified no served value and must exit non-zero.
func TestServedValuesVerdict_ListCheckIsNotAValueCheck(t *testing.T) {
	ok := servedValueResult{name: "v", ok: true}
	dark := servedValueResult{name: "v", relErr: math.NaN(), skipped: true}
	drifted := servedValueResult{name: "v"}
	listOK := &reserveListResult{}
	listDark := &reserveListResult{skipped: true}
	listDrift := &reserveListResult{drift: reserveListDrift{extra: []string{"GAAA"}}}
	cases := []struct {
		name    string
		results []servedValueResult
		list    *reserveListResult
		wantErr bool
	}{
		{"every value source dark, list verified: verified no served value", []servedValueResult{dark, dark, dark}, listOK, true},
		{"every value source dark, list dark", []servedValueResult{dark, dark, dark}, listDark, true},
		{"every value source dark, no list", []servedValueResult{dark, dark, dark}, nil, true},
		{"values verified, list dark: a lone outage stays clean", []servedValueResult{ok, ok, ok}, listDark, false},
		{"one value verified, list verified", []servedValueResult{dark, ok, dark}, listOK, false},
		{"list drift fails the run", []servedValueResult{ok, ok, ok}, listDrift, true},
		{"value drift fails the run", []servedValueResult{ok, drifted, ok}, listOK, true},
		{"all clean", []servedValueResult{ok, ok, ok}, listOK, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := servedValuesVerdict(io.Discard, tc.results, tc.list); tc.wantErr != (err != nil) {
				t.Fatalf("err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// TestRunServedValueChecks_Recheck pins the snapshot-race handling: a drift
// re-reads both sides, passes if the gap clears, and fails only when it
// persists, naming a stale served snapshot when supply_as_of_ledger did not
// advance. The 2^53 row passes under a float64 comparison (both sides round to
// the same double) and must fail exactly.
func TestRunServedValueChecks_Recheck(t *testing.T) {
	type read struct {
		served string
		ledger uint32
		truth  string
	}
	cases := []struct {
		name      string
		tolerance float64
		reads     []read
		wantOK    bool
		wantNote  string
		wantReads int
	}{
		{"transient gap clears on re-read", 0.02, []read{{"100", 1000, "97"}, {"97", 1001, "97"}}, true, "cleared on recheck", 2},
		{"persistent gap with advanced ledger fails", 0.02, []read{{"100", 1000, "97"}, {"100", 1001, "97"}}, false, "gap persisted after the served snapshot advanced", 2},
		{"persistent gap on a stale snapshot fails as stale", 0.02, []read{{"100", 1000, "97"}, {"100", 1000, "97"}}, false, "served supply snapshot stale", 2},
		{"in tolerance never rechecks", 0.02, []read{{"100", 1000, "99"}}, true, "probe note", 1},
		{"one base unit above 2^53 is a drift", 0, []read{{"9007199254740993", 1000, "9007199254740992"}, {"9007199254740993", 1001, "9007199254740992"}}, false, "gap persisted", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var n atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				rd := tc.reads[min(int(n.Add(1)), len(tc.reads))-1]
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"data":{"total_supply":%q,"supply_as_of_ledger":%d}}`, rd.served, rd.ledger)
			}))
			t.Cleanup(api.Close)
			check := servedValueCheck{
				name: "probe", tolerance: tc.tolerance, note: "probe note",
				served: servedSupplyField("native", "total_supply", 0),
				// The truth side follows the served read it is paired with.
				truth: func(context.Context, *http.Client) (*big.Rat, error) {
					return decimalRat(json.RawMessage(tc.reads[min(int(n.Load()), len(tc.reads))-1].truth))
				},
			}
			r := runChecksForTest(context.Background(), api.URL, []servedValueCheck{check})[0]
			if r.ok != tc.wantOK || r.skipped {
				t.Errorf("ok=%v skipped=%v, want ok=%v (note=%s)", r.ok, r.skipped, tc.wantOK, r.note)
			}
			if !strings.Contains(r.note, tc.wantNote) {
				t.Errorf("note = %q, want it to contain %q", r.note, tc.wantNote)
			}
			if got := int(n.Load()); got != tc.wantReads {
				t.Errorf("served reads = %d, want %d", got, tc.wantReads)
			}
		})
	}
}

// TestRunServedValueChecks_RecheckFailuresStayFailed — a re-read that cannot
// complete (truth dark, or the run cancelled during the wait) keeps the first
// drift verdict rather than turning it into a skip or a pass.
func TestRunServedValueChecks_RecheckFailuresStayFailed(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"total_supply":"100","supply_as_of_ledger":1000}}`))
	}))
	t.Cleanup(api.Close)
	var truthCalls atomic.Int32
	check := servedValueCheck{
		name: "probe", tolerance: 0.02,
		served: servedSupplyField("native", "total_supply", 0),
		truth: func(context.Context, *http.Client) (*big.Rat, error) {
			if truthCalls.Add(1) > 1 {
				return nil, errors.New("down")
			}
			return big.NewRat(90, 1), nil
		},
	}
	r := runChecksForTest(context.Background(), api.URL, []servedValueCheck{check})[0]
	if r.ok || r.skipped || !strings.Contains(r.note, "recheck read failed") {
		t.Errorf("dark truth on recheck: ok=%v skipped=%v note=%q, want the first drift verdict", r.ok, r.skipped, r.note)
	}

	truthCalls.Store(0)
	check.recheckAfter = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	r = runServedValueChecks(ctx, &http.Client{}, api.URL, []servedValueCheck{check}, 5*time.Second, io.Discard)[0]
	if r.ok || r.skipped || !strings.Contains(r.note, "recheck aborted") {
		t.Errorf("cancelled wait: ok=%v skipped=%v note=%q, want the first drift verdict", r.ok, r.skipped, r.note)
	}
}

// TestDecimalRat_ExactAbove2Pow53 — both truth-side encodings (quoted string
// and bare JSON number) parse exactly where float64 would round.
func TestDecimalRat_ExactAbove2Pow53(t *testing.T) {
	want, _ := new(big.Rat).SetString("90071992547409930000001.0000001")
	for _, raw := range []string{`"90071992547409930000001.0000001"`, `90071992547409930000001.0000001`} {
		got, err := decimalRat(json.RawMessage(raw))
		if err != nil || got.Cmp(want) != 0 {
			t.Errorf("decimalRat(%s) = %v, %v; want %s", raw, got, err, want.RatString())
		}
	}
	if _, err := decimalRat(json.RawMessage(`{"x":1}`)); err == nil {
		t.Error("an object must not parse as a decimal")
	}
}
