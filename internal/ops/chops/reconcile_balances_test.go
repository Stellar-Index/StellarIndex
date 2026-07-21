// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"context"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// ─── xlmDecimalToStroops: exact decimal parse, no float64 ─────────────────

func TestXLMDecimalToStroops(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string // decimal string, compared via big.Int
		wantErr bool
	}{
		{"whole-and-fraction", "1.5", "15000000", false},
		{"smallest-unit", "0.0000001", "1", false},
		{"large-fully-padded", "1000000.0000000", "10000000000000", false},
		{"zero", "0", "0", false},
		{"zero-with-fraction", "0.0000000", "0", false},
		{"no-fraction", "42", "420000000", false},
		{"over-precision-truncates-not-errors", "1.00000009", "10000000", false}, // 9th digit dropped, matches scale.DecimalStringToScaledInt's documented truncation
		{"empty-string-errors", "", "", true},
		{"garbage-errors", "not-a-number", "", true},
		{"scientific-notation-errors", "1.5e10", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := xlmDecimalToStroops(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("xlmDecimalToStroops(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("xlmDecimalToStroops(%q) unexpected error: %v", tc.in, err)
			}
			want, ok := new(big.Int).SetString(tc.want, 10)
			if !ok {
				t.Fatalf("test bug: %q is not a valid big.Int literal", tc.want)
			}
			if got.Cmp(want) != 0 {
				t.Fatalf("xlmDecimalToStroops(%q) = %s, want %s", tc.in, got.String(), want.String())
			}
		})
	}
}

// TestXLMDecimalToStroops_NeverRoutesThroughFloat64 pins the exact-integer
// property that motivates reusing scale.DecimalStringToScaledInt at all
// (ADR-0003): a value with more significant digits than float64's 53-bit
// mantissa can represent exactly must still round-trip losslessly. XLM's
// real total supply (~1.05e18 stroops) is comfortably inside this range but
// this pins the parser itself, independent of realistic XLM magnitudes.
func TestXLMDecimalToStroops_NeverRoutesThroughFloat64(t *testing.T) {
	// 2^53 + 1 = 9007199254740993 is the canonical "smallest integer a
	// float64 cannot represent exactly" probe value.
	got, err := xlmDecimalToStroops("900719925.4740993")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := big.NewInt(9007199254740993)
	if got.Cmp(want) != 0 {
		t.Fatalf("xlmDecimalToStroops lost precision: got %s, want %s", got.String(), want.String())
	}
}

// ─── bigStroopsToInt64 ──────────────────────────────────────────────────

func TestBigStroopsToInt64(t *testing.T) {
	v, err := bigStroopsToInt64(big.NewInt(15000000))
	if err != nil || v != 15000000 {
		t.Fatalf("bigStroopsToInt64(15000000) = (%d, %v), want (15000000, nil)", v, err)
	}

	overflow := new(big.Int).Lsh(big.NewInt(1), 100) // way past int64
	if _, err := bigStroopsToInt64(overflow); err == nil {
		t.Fatalf("bigStroopsToInt64(2^100) should have errored on overflow")
	}
}

// ─── classifyBalances: MATCH / MISMATCH + delta ────────────────────────────

func TestClassifyBalances(t *testing.T) {
	cases := []struct {
		name                string
		our, ref, tolerance int64
		wantOutcome         reconcileOutcome
		wantDelta           int64
	}{
		{"exact-match", 15000000, 15000000, 0, outcomeMatch, 0},
		{"ref-ahead-mismatch", 15000000, 15000001, 0, outcomeMismatch, 1},
		{"our-ahead-mismatch", 15000001, 15000000, 0, outcomeMismatch, 1},
		{"within-tolerance", 15000000, 15000005, 5, outcomeMatch, 5},
		{"just-outside-tolerance", 15000000, 15000006, 5, outcomeMismatch, 6},
		{"both-zero", 0, 0, 0, outcomeMatch, 0},
		{"large-delta", 1_000_000_000_000, 1, 0, outcomeMismatch, 999_999_999_999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outcome, delta := classifyBalances(tc.our, tc.ref, tc.tolerance)
			if outcome != tc.wantOutcome || delta != tc.wantDelta {
				t.Fatalf("classifyBalances(%d,%d,%d) = (%s,%d), want (%s,%d)",
					tc.our, tc.ref, tc.tolerance, outcome, delta, tc.wantOutcome, tc.wantDelta)
			}
		})
	}
}

// ─── parseHorizonNativeBalance: mocked response bodies, no network ────────

func TestParseHorizonNativeBalance(t *testing.T) {
	body := []byte(`{
		"balances": [
			{"asset_type": "credit_alphanum4", "asset_code": "USDC", "balance": "123.4560000"},
			{"asset_type": "native", "balance": "1234.5670000"}
		]
	}`)
	got, err := parseHorizonNativeBalance(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := big.NewInt(12345670000)
	if got.Cmp(want) != 0 {
		t.Fatalf("parseHorizonNativeBalance = %s, want %s", got.String(), want.String())
	}
}

func TestParseHorizonNativeBalance_NoNativeEntry(t *testing.T) {
	body := []byte(`{"balances": [{"asset_type": "credit_alphanum4", "balance": "1.0000000"}]}`)
	if _, err := parseHorizonNativeBalance(body); err == nil {
		t.Fatalf("expected errNoNativeBalance, got nil")
	}
}

func TestParseHorizonNativeBalance_MalformedJSON(t *testing.T) {
	if _, err := parseHorizonNativeBalance([]byte(`not json`)); err == nil {
		t.Fatalf("expected a parse error, got nil")
	}
}

// ─── fetchHorizonNativeBalance: httptest server, no real network ──────────

func TestFetchHorizonNativeBalance_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/accounts/GABC") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balances":[{"asset_type":"native","balance":"42.0000001"}]}`))
	}))
	defer srv.Close()

	bal, status, err := fetchHorizonNativeBalance(context.Background(), srv.Client(), srv.URL, "GABC")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if want := big.NewInt(420000001); bal.Cmp(want) != 0 {
		t.Fatalf("balance = %s, want %s", bal.String(), want.String())
	}
}

func TestFetchHorizonNativeBalance_404IsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status": 404, "detail": "Resource Missing"}`))
	}))
	defer srv.Close()

	bal, status, err := fetchHorizonNativeBalance(context.Background(), srv.Client(), srv.URL, "GDEAD")
	if err != nil {
		t.Fatalf("404 should not surface as an error, got: %v", err)
	}
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if bal != nil {
		t.Fatalf("balance should be nil on 404, got %v", bal)
	}
}

func TestFetchHorizonNativeBalance_429ThenSuccess(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1") // smallest real Retry-After horizonRetryAfter honours, so this test's one unavoidable sleep stays short
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balances":[{"asset_type":"native","balance":"1.0000000"}]}`))
	}))
	defer srv.Close()

	// fetchHorizonNativeBalance's 429 branch really does sleep for the
	// backoff duration (time.After, no injectable clock) — accept the
	// one bounded 1s real sleep here rather than adding test-only clock
	// plumbing to production code for a single retry test.
	bal, status, err := fetchHorizonNativeBalance(context.Background(), srv.Client(), srv.URL, "GRATE")
	if err != nil {
		t.Fatalf("unexpected error after 429-then-200: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (one 429, one success)", calls)
	}
	if want := big.NewInt(10000000); bal.Cmp(want) != 0 {
		t.Fatalf("balance = %s, want %s", bal.String(), want.String())
	}
}

func TestHorizonRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // formatted duration, for a readable failure message
	}{
		{"absent", "", "3s"},
		{"garbage", "soon", "3s"},
		{"zero", "0", "3s"},
		{"normal", "5", "5s"},
		{"capped-at-30s", "3600", "30s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := horizonRetryAfter(tc.in); got.String() != tc.want {
				t.Fatalf("horizonRetryAfter(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// ─── sampleConfirmedNothing: F4 vacuous-sample fail-open guard ─────────────

func TestSampleConfirmedNothing(t *testing.T) {
	cases := []struct {
		name    string
		results []reconcileResult
		want    bool
	}{
		{"empty is not a confirm-nothing (guarded elsewhere)", nil, false},
		{"one match confirms something", []reconcileResult{{Outcome: outcomeMatch}}, false},
		{"a match among no-data confirms something", []reconcileResult{
			{Outcome: outcomeNoData}, {Outcome: outcomeMatch}, {Outcome: outcomeMergedOrAbsent},
		}, false},
		{"all no-data confirmed nothing", []reconcileResult{
			{Outcome: outcomeNoData}, {Outcome: outcomeNoData},
		}, true},
		{"all merged/absent confirmed nothing", []reconcileResult{
			{Outcome: outcomeMergedOrAbsent}, {Outcome: outcomeMergedOrAbsent},
		}, true},
		{"all errored confirmed nothing (C2-15 also catches this)", []reconcileResult{
			{Outcome: outcomeError}, {Outcome: outcomeError},
		}, true},
		{"a mismatch is not a match — still confirmed nothing MATCHED", []reconcileResult{
			{Outcome: outcomeMismatch}, {Outcome: outcomeNoData},
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sampleConfirmedNothing(tc.results); got != tc.want {
				t.Fatalf("sampleConfirmedNothing = %v, want %v", got, tc.want)
			}
		})
	}
}

// ─── printReconcileReport: exit-code-bearing summary ───────────────────────

func TestPrintReconcileReport_CountsAndExitCode(t *testing.T) {
	results := []reconcileResult{
		{Account: "G1", Outcome: outcomeMatch},
		{Account: "G2", Outcome: outcomeMismatch, OurStroops: 100, RefStroops: 90, Delta: 10},
		{Account: "G3", Outcome: outcomeMismatch, OurStroops: 5, RefStroops: 0, Delta: 5},
		{Account: "G4", Outcome: outcomeNoData},
		{Account: "G5", Outcome: outcomeMergedOrAbsent},
	}
	var buf bytes.Buffer
	got, errored := printReconcileReport(&buf, results)
	if got != 2 {
		t.Fatalf("printReconcileReport mismatches = %d, want 2 (the exit code)", got)
	}
	if errored != 0 {
		t.Fatalf("printReconcileReport errored = %d, want 0", errored)
	}
	out := buf.String()
	for _, want := range []string{"G2", "G3", "MATCHED", "MISMATCH", "NO_DATA", "MERGED_OR_ABSENT"} {
		if !strings.Contains(out, want) {
			t.Fatalf("report missing %q; full report:\n%s", want, out)
		}
	}
}

func TestPrintReconcileReport_AllMatchZeroExitCode(t *testing.T) {
	results := []reconcileResult{
		{Account: "G1", Outcome: outcomeMatch},
		{Account: "G2", Outcome: outcomeMatch},
	}
	var buf bytes.Buffer
	if got, errored := printReconcileReport(&buf, results); got != 0 || errored != 0 {
		t.Fatalf("printReconcileReport = (%d mismatch, %d error), want (0, 0)", got, errored)
	}
}

// TestPrintReconcileReport_CountsErrors pins the input to the C2-15 fail-open
// guard: the report must surface the ERROR count so a mostly-errored run can be
// failed (an all-errored run verified nothing and must NOT exit clean).
func TestPrintReconcileReport_CountsErrors(t *testing.T) {
	results := []reconcileResult{
		{Account: "G1", Outcome: outcomeError},
		{Account: "G2", Outcome: outcomeError},
		{Account: "G3", Outcome: outcomeMatch},
	}
	var buf bytes.Buffer
	mismatches, errored := printReconcileReport(&buf, results)
	if mismatches != 0 {
		t.Fatalf("mismatches = %d, want 0", mismatches)
	}
	if errored != 2 {
		t.Fatalf("errored = %d, want 2 (the C2-15 guard fails the run on this)", errored)
	}
	if !strings.Contains(buf.String(), "ERROR") {
		t.Fatalf("report should surface the ERROR count; got:\n%s", buf.String())
	}
}

// TestReconcileExitError pins the consolidated exit decision (reviewer #4: the
// C2-15 and F4 exit branches were previously untested at the command level) and
// the C2-15 Horizon-split (reviewer #2: truth outages are outcomeTruthUnavailable
// and NOT in `errored`, so a Horizon rate-limit episode can't fail a healthy
// -sample gate).
func TestReconcileExitError(t *testing.T) {
	cases := []struct {
		name                     string
		mismatches, errored, n   int
		haveSample, confirmedNil bool // confirmedNil = sampleConfirmedNothing
		maxErrorRate             float64
		wantExit                 bool
		wantCode                 int // only checked when wantExit
	}{
		{"clean sample pass", 0, 0, 100, true, false, 0.25, false, 0},
		{"mismatches exit with count", 3, 0, 100, true, false, 0.25, true, 3},
		{"mismatch count capped at 255", 900, 0, 1000, true, false, 0.25, true, 255},
		// C2-15: our-side error rate
		{"our-error rate over threshold fails even at 0 mismatch", 0, 30, 100, true, false, 0.25, true, 255},
		{"our-error rate at threshold stays clean", 0, 25, 100, true, false, 0.25, false, 0},
		{"our-error over threshold WITH mismatches keeps mismatch code", 4, 30, 100, true, false, 0.25, true, 4},
		// C2-15 Horizon-split: truth-unavailable is NOT in `errored`, so a run
		// with 70 match + 30 truth-dark has errored=0 → passes the rate guard.
		{"30% Horizon-dark does NOT trip C2-15 (errored=0)", 0, 0, 100, true, false, 0.25, false, 0},
		// F4: -sample matched nothing
		{"sample matched nothing fails", 0, 0, 100, true, true, 0.25, true, 255},
		{"single -account matched nothing is exempt", 0, 0, 1, false, true, 0.25, false, 0},
		{"empty run is clean", 0, 0, 0, true, false, 0.25, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err, _ := reconcileExitError(tc.mismatches, tc.errored, tc.n, tc.haveSample, tc.confirmedNil, tc.maxErrorRate)
			if tc.wantExit != (err != nil) {
				t.Fatalf("reconcileExitError = %v, wantExit=%v", err, tc.wantExit)
			}
			if tc.wantExit {
				ece, ok := err.(*opsutil.ExitCodeError)
				if !ok {
					t.Fatalf("want *ExitCodeError, got %T", err)
				}
				if ece.Code != tc.wantCode {
					t.Fatalf("exit code = %d, want %d", ece.Code, tc.wantCode)
				}
			}
		})
	}
}

// TestPrintReconcileReport_TruthUnavailableNotCountedAsError pins that a
// truth-source outage is tallied separately and NOT returned as an our-side
// error (so it can't feed the C2-15 rate).
func TestPrintReconcileReport_TruthUnavailableNotCountedAsError(t *testing.T) {
	results := []reconcileResult{
		{Account: "G1", Outcome: outcomeMatch},
		{Account: "G2", Outcome: outcomeTruthUnavailable},
		{Account: "G3", Outcome: outcomeTruthUnavailable},
		{Account: "G4", Outcome: outcomeError},
	}
	var buf bytes.Buffer
	mismatches, errored := printReconcileReport(&buf, results)
	if mismatches != 0 {
		t.Fatalf("mismatches = %d, want 0", mismatches)
	}
	if errored != 1 {
		t.Fatalf("errored = %d, want 1 (only the CH-side ERROR; the 2 TRUTH_UNAVAILABLE excluded)", errored)
	}
	if out := buf.String(); !strings.Contains(out, "TRUTH_UNAVAILABLE") {
		t.Fatalf("report should surface TRUTH_UNAVAILABLE; got:\n%s", out)
	}
}
