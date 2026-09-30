// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── lakeExitCode: capped exit-code aggregation, no live CH ───────────────

func TestLakeExitCode(t *testing.T) {
	cases := []struct {
		name                                  string
		gaps, deficiency, broken, censusShort uint64
		want                                  int
	}{
		{"all-zero", 0, 0, 0, 0, 0},
		{"gaps-only", 3, 0, 0, 0, 3},
		{"deficiency-only", 0, 5, 0, 0, 5},
		{"broken-only", 0, 0, 2, 0, 2},
		{"census-only", 0, 0, 0, 1, 1},
		{"sums-all-four", 10, 20, 30, 40, 100},
		{"exactly-255-not-capped", 100, 100, 55, 0, 255},
		{"just-over-cap", 100, 100, 55, 1, 255},
		{"way-over-cap", 1_000_000, 0, 0, 0, 255},
		{"sum-overflow-guard-still-caps", 200, 200, 200, 200, 255},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lakeExitCode(tc.gaps, tc.deficiency, tc.broken, tc.censusShort); got != tc.want {
				t.Fatalf("lakeExitCode(%d,%d,%d,%d) = %d, want %d", tc.gaps, tc.deficiency, tc.broken, tc.censusShort, got, tc.want)
			}
		})
	}
}

// ─── resolveVerifyTo: fail-closed against an independent tip (GH-1180) ────

func TestResolveVerifyTo(t *testing.T) {
	cases := []struct {
		name               string
		chMax, independent uint32
		wantTo             uint32
		wantErr            bool
	}{
		{"chMax at or ahead of independent tip passes through", 63_100_000, 63_099_000, 63_100_000, false},
		{"within tolerance is expected lag, not truncation", 63_100_000, 63_100_050, 63_100_000, false},
		{"three partitions short of the independent tip fails closed", 60_000_000, 63_000_000, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveVerifyTo("verify-lake", "ledgerstream cursor", tc.chMax, tc.independent)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveVerifyTo(%d,%d) = %d, <nil>, want an error (truncated lake must fail closed)", tc.chMax, tc.independent, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveVerifyTo(%d,%d) unexpected error: %v", tc.chMax, tc.independent, err)
			}
			if got != tc.wantTo {
				t.Fatalf("resolveVerifyTo(%d,%d) = %d, want %d", tc.chMax, tc.independent, got, tc.wantTo)
			}
		})
	}
}

func TestResolveVerifyToTips(t *testing.T) {
	const chMax = 63_100_000
	down := errors.New("unreachable")
	cases := []struct {
		name    string
		tips    []independentTip
		wantErr string
		wantLog string
	}{
		{
			"cursor stalled with the lake, archive ahead fails closed",
			[]independentTip{{source: "ledgerstream cursor", seq: chMax}, {source: "history archive", seq: chMax + 10_000}},
			"history archive", "",
		},
		{
			"archive a checkpoint behind the lake passes",
			[]independentTip{{source: "ledgerstream cursor", seq: chMax}, {source: "history archive", seq: chMax - 64}},
			"", "",
		},
		{
			"archive unreachable still checks the cursor",
			[]independentTip{{source: "ledgerstream cursor", seq: chMax + 5_000}, {source: "history archive", err: down}},
			"ledgerstream cursor", "",
		},
		{
			"cursor unreachable still checks the archive",
			[]independentTip{{source: "ledgerstream cursor", err: down}, {source: "history archive", seq: chMax + 5_000}},
			"history archive", "ledgerstream cursor unavailable",
		},
		{
			"no tip readable falls back to ClickHouse max",
			[]independentTip{{source: "ledgerstream cursor", err: down}, {source: "history archive", err: down}},
			"", "no independent tip available",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var warn strings.Builder
			got, err := resolveVerifyToTips("verify-lake", chMax, tc.tips, &warn)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("resolveVerifyToTips = %d, %v; want an error naming %q", got, err, tc.wantErr)
				}
			} else if err != nil || got != chMax {
				t.Fatalf("resolveVerifyToTips = %d, %v; want %d, <nil>", got, err, chMax)
			}
			if tc.wantLog != "" && !strings.Contains(warn.String(), tc.wantLog) {
				t.Fatalf("warning %q does not contain %q", warn.String(), tc.wantLog)
			}
		})
	}
}

func TestHistoryArchiveTip(t *testing.T) {
	serve := func(status int, body string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/archive/.well-known/stellar-history.json" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	srv := serve(http.StatusOK, `{"version":1,"currentLedger":63110015}`)
	got, err := historyArchiveTip(context.Background(), srv.URL+"/archive")
	if err != nil || got != 63_110_015 {
		t.Fatalf("historyArchiveTip = %d, %v; want 63110015, <nil>", got, err)
	}

	for name, srv := range map[string]*httptest.Server{
		"zero currentLedger": serve(http.StatusOK, `{}`),
		"HTTP 503":           serve(http.StatusServiceUnavailable, "down"),
	} {
		if got, err := historyArchiveTip(context.Background(), srv.URL+"/archive"); err == nil {
			t.Fatalf("%s: historyArchiveTip = %d, <nil>; want an error", name, got)
		}
	}
	if _, err := historyArchiveTip(context.Background(), ""); err == nil {
		t.Fatal(`historyArchiveTip("") = <nil>; want an error`)
	}
}

// ─── lakeCheckLine / lakeChecksRunLabel: SKIPPED vs. zero (GH-1195) ───────

func TestLakeCheckLineDistinguishesSkippedFromZero(t *testing.T) {
	skipped := lakeCheckLine("hash_chain", false, "0 broken link(s)")
	if !strings.Contains(skipped, "SKIPPED") {
		t.Fatalf("lakeCheckLine(ran=false) = %q, want it to say SKIPPED, not the zero-valued detail", skipped)
	}
	ran := lakeCheckLine("hash_chain", true, "0 broken link(s)")
	if strings.Contains(ran, "SKIPPED") || !strings.Contains(ran, "0 broken link(s)") {
		t.Fatalf("lakeCheckLine(ran=true) = %q, want the detail, not SKIPPED", ran)
	}
}

func TestLakeChecksRunLabel(t *testing.T) {
	if got := lakeChecksRunLabel(lakeChecks{contiguity: true, hashChain: true}); got != "contiguity,hashchain" {
		t.Fatalf("lakeChecksRunLabel(contiguity,hashchain) = %q", got)
	}
	if got := lakeChecksRunLabel(lakeChecks{entryChanges: true, rawCensus: true}); got != "entrychanges,rawcensus" {
		t.Fatalf("lakeChecksRunLabel(entrychanges,rawcensus) = %q", got)
	}
	if got := lakeChecksRunLabel(lakeChecks{}); got != "none" {
		t.Fatalf("lakeChecksRunLabel(none) = %q, want %q", got, "none")
	}
}

// ─── parseLakeChecks: -checks flag parsing, no live CH ────────────────────

func TestParseLakeChecks(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    lakeChecks
		wantErr bool
	}{
		{"default-all-four", lakeChecksDefault, lakeChecks{true, true, true, true}, false},
		{"single-contiguity", "contiguity", lakeChecks{contiguity: true}, false},
		{"single-entrychanges", "entrychanges", lakeChecks{entryChanges: true}, false},
		{"single-hashchain", "hashchain", lakeChecks{hashChain: true}, false},
		{"single-rawcensus", "rawcensus", lakeChecks{rawCensus: true}, false},
		{"two-of-four", "contiguity,hashchain", lakeChecks{contiguity: true, hashChain: true}, false},
		{"whitespace-tolerant", " contiguity , hashchain ", lakeChecks{contiguity: true, hashChain: true}, false},
		{"unknown-token-errors", "contiguity,bogus", lakeChecks{}, true},
		{"empty-string-errors", "", lakeChecks{}, true},
		{"only-commas-errors", ",,", lakeChecks{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLakeChecks(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseLakeChecks(%q) = nil error, want error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLakeChecks(%q) unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("parseLakeChecks(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
		})
	}
}

// ─── renderLakeVerifyProm: textfile body, no live CH ──────────────────────

func TestRenderLakeVerifyProm(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	body := renderLakeVerifyProm(
		lakeChecks{contiguity: true, rawCensus: true},
		lakeCounts{gaps: 0, ecDeficiency: 7, hashChainBroken: 9, censusShort: 3},
		2, 61_000_000, now)

	for _, want := range []string{
		"stellarindex_lake_verify_failures{check=\"contiguity\"} 0\n",
		"stellarindex_lake_verify_failures{check=\"raw_census\"} 3\n",
		"stellarindex_lake_verify_from_ledger 2\n",
		"stellarindex_lake_verify_to_ledger 61000000\n",
		"stellarindex_lake_verify_last_run_unix 1790000000\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("textfile missing %q:\n%s", want, body)
		}
	}
	// A check that did not run must not publish a zero that reads as a pass.
	for _, absent := range []string{`check="entry_changes"`, `check="hash_chain"`} {
		if strings.Contains(body, absent) {
			t.Errorf("narrowed run emitted %s:\n%s", absent, body)
		}
	}
	for _, fam := range []string{
		"stellarindex_lake_verify_failures",
		"stellarindex_lake_verify_from_ledger",
		"stellarindex_lake_verify_to_ledger",
		"stellarindex_lake_verify_last_run_unix",
	} {
		if n := strings.Count(body, "# TYPE "+fam+" gauge\n"); n != 1 {
			t.Errorf("# TYPE %s appears %d times, want 1", fam, n)
		}
	}
	if !strings.HasSuffix(body, "\n") {
		t.Errorf("textfile must end in a newline:\n%s", body)
	}
}
